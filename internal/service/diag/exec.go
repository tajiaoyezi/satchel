package diag

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/satchel/satchel/internal/command"
	coreaudit "github.com/satchel/satchel/internal/core/audit"
	corejobs "github.com/satchel/satchel/internal/core/jobs"
	coreservers "github.com/satchel/satchel/internal/core/servers"
	v1 "github.com/satchel/satchel/pkg/api/v1"
	"github.com/satchel/satchel/pkg/xrpc"
)

const (
	execJobKind          = "server exec"
	defaultExecTimeout   = 30 * time.Second
	minExecTimeout       = 1 * time.Second
	maxExecTimeout       = 300 * time.Second
	execPreviewMaxBytes  = 2048
	execReconnectGrace   = 5 * time.Second
	execReconcileTimeout = 5 * time.Minute
)

// ExecJobResult 是 server exec 写入 job.output 的结构化执行结果对象。
type ExecJobResult struct {
	Server        string `json:"server"`
	ServerID      int64  `json:"server_id"`
	Command       string `json:"command"`
	State         string `json:"state"`
	ExitCode      *int   `json:"exit_code,omitempty"`
	Output        string `json:"output"`
	Truncated     bool   `json:"output_truncated,omitempty"`
	DurationMS    int64  `json:"duration_ms,omitempty"`
	Error         string `json:"error,omitempty"`
	UnknownReason string `json:"unknown_reason,omitempty"`
	Reconciled    bool   `json:"reconciled,omitempty"`
}

type queuedExec struct {
	ready      chan struct{}
	serverName string

	mu       sync.Mutex
	reportFn func(pos int)
}

func (q *queuedExec) setReporter(fn func(pos int)) {
	q.mu.Lock()
	q.reportFn = fn
	q.mu.Unlock()
}

func (q *queuedExec) updatePosition(pos int) {
	q.mu.Lock()
	fn := q.reportFn
	q.mu.Unlock()
	if fn != nil {
		fn(pos)
	}
}

type execTrack struct {
	jobID      string
	serverID   int64
	serverName string
	command    string
	actor      v1.Identity

	mu        sync.Mutex
	buf       bytes.Buffer
	truncated bool
	done      bool
	state     string
	exitCode  int
	durMS     int64
	errMsg    string
	offline   bool
	notifyCh  chan struct{}
}

func (t *execTrack) signal() {
	select {
	case t.notifyCh <- struct{}{}:
	default:
	}
}

func (t *execTrack) appendFromOffset(startOff int, chunk string, truncated bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if truncated {
		t.truncated = true
	}
	if chunk != "" {
		if startOff < 0 {
			startOff = 0
		}
		curLen := t.buf.Len()
		endOff := startOff + len(chunk)
		if endOff > curLen {
			skip := curLen - startOff
			if skip < 0 {
				skip = 0
			}
			if skip < len(chunk) {
				rem := xrpc.ExecMaxOutputBytes - t.buf.Len()
				toWrite := chunk[skip:]
				if rem <= 0 {
					t.truncated = true
				} else if len(toWrite) > rem {
					_, _ = t.buf.WriteString(toWrite[:rem])
					t.truncated = true
				} else {
					_, _ = t.buf.WriteString(toWrite)
				}
			}
		}
	}
}

func (t *execTrack) replaceFullOutput(full string, truncated bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(full) > xrpc.ExecMaxOutputBytes {
		full = full[:xrpc.ExecMaxOutputBytes]
		truncated = true
	}
	if len(full) >= t.buf.Len() {
		t.buf.Reset()
		_, _ = t.buf.WriteString(full)
	}
	if truncated {
		t.truncated = true
	}
}

func isLoopbackIP(raw string) bool {
	s := strings.TrimSpace(raw)
	if s == "" {
		return false
	}
	if host, _, err := net.SplitHostPort(s); err == nil {
		s = host
	}
	ip := net.ParseIP(s)
	return ip != nil && ip.IsLoopback()
}

func parseExecTimeout(raw string) (time.Duration, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return defaultExecTimeout, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return 0, v1.Newf(v1.CodeBadRequest, "--timeout 值 %q 不是合法时长（如 30s、2m）", raw)
	}
	if d < minExecTimeout || d > maxExecTimeout {
		return 0, v1.Newf(v1.CodeBadRequest, "--timeout 必须在 %s..%s 之间，得到 %s", minExecTimeout, maxExecTimeout, d)
	}
	return d, nil
}

func (s *Service) serverExec(ctx context.Context, inv *command.Invocation) (any, error) {
	if err := requireAdmin(ctx); err != nil {
		return nil, err
	}
	srv, err := s.resolveServer(ctx, inv.Arg(0))
	if err != nil {
		return nil, err
	}

	cmdText := strings.TrimSpace(inv.String("command", ""))
	if cmdText == "" {
		return nil, v1.New(v1.CodeBadRequest, "必须通过 --command 或 -- <命令> 指定要执行的命令")
	}
	if len(cmdText) > xrpc.ExecMaxCommandBytes {
		return nil, v1.Newf(v1.CodeBadRequest, "命令长度超出上限 %d 字节", xrpc.ExecMaxCommandBytes)
	}
	timeout, err := parseExecTimeout(inv.String("timeout", ""))
	if err != nil {
		return nil, err
	}

	// 闸门 2（前哨）：已标同机的节点直接永久拒绝。
	if srv.Status.SameHostAsMaster {
		return nil, v1.Newf(v1.CodeForbidden, "服务器 %q 与主控同机，同机节点永久禁止执行 server exec", srv.Spec.Name).
			WithNext("同机排障请使用只读 server diag 或在宿主机本机运维")
	}

	// 闸门 4：离线节点直接拒绝，绝不入队等待。
	connectedWS := s.deps.Nodes.ConnectedWS(srv.Metadata.ID)
	rt, hasRT := s.deps.Nodes.NodeRuntime(srv.Metadata.ID)
	if !connectedWS {
		if srv.Status.Status != coreservers.StatusConnected && srv.Spec.ConnectionMode != coreservers.ModePull {
			return nil, v1.Newf(v1.CodeUnavailable, "服务器 %q 当前不在线，server exec 不允许离线入队", srv.Spec.Name).
				WithNext("待节点重新连上主控后再重试")
		}
		// HTTP / pull 模式下实时探测 agent.info 以确认可达并刷新 machine_id / exec_enabled / peer_ip。
		info, infoErr := s.deps.Nodes.CallAgentInfo(ctx, srv.Metadata.ID)
		if infoErr != nil {
			return nil, infoErr
		}
		rt, hasRT = s.deps.Nodes.NodeRuntime(srv.Metadata.ID)
		if info != nil {
			if rt.MachineID == "" {
				rt.MachineID = strings.TrimSpace(info.MachineID)
			}
			rt.ExecEnabled = info.ExecEnabled
			hasRT = true
		}
	}

	// 闸门 2（核心）：同机 fail-closed 判定——主控 machine_id、节点 machine_id、连接对端 IP 任一缺失，
	// 或对端为回环地址、或 machine_id 与主控相同，一律按同机处理并拒绝。
	masterMID := strings.TrimSpace(s.deps.Nodes.MasterMachineID())
	nodeMID := strings.TrimSpace(rt.MachineID)
	peerIP := strings.TrimSpace(rt.PeerIP)
	if !hasRT || masterMID == "" || nodeMID == "" || peerIP == "" {
		return nil, v1.Newf(v1.CodeForbidden, "服务器 %q 缺少完整的运行时同机判定证据（主控 machine_id、节点 machine_id 或来源 IP 缺失），按 fail-closed 原则拒绝 server exec", srv.Spec.Name).
			WithNext("请确认主控与节点均为带 /etc/machine-id 的独立 Linux 主机并已重新完成认证")
	}
	if isLoopbackIP(peerIP) || nodeMID == masterMID {
		return nil, v1.Newf(v1.CodeForbidden, "服务器 %q 判定与主控同机（来源回环或 machine_id 相同），永久禁止执行 server exec", srv.Spec.Name).
			WithNext("同机排障请使用只读 server diag 或在宿主机本机运维")
	}

	// 闸门 3：节点本地单向开关 allow_exec 必须已在本机开启。
	if !rt.ExecEnabled {
		return nil, v1.Newf(v1.CodeForbidden, "服务器 %q 本地未开启 allow_exec，拒绝执行 server exec", srv.Spec.Name).
			WithNext("若确认需要远程执行，请登录该节点本机在 config.yaml 中设置 allow_exec: true（或重新执行 join --allow-exec）并重启 satchel-agent")
	}

	// 并发上限与排队深度控制（在建 job 之前同步预占，排队满直接 conflict 且不建 job）。
	limits := s.loadDiagLimits(ctx)
	var (
		qe       *queuedExec
		queuePos int
		acquired bool
	)
	s.execMu.Lock()
	if s.runningCnt < limits.ExecMaxConcurrency {
		s.runningCnt++
		acquired = true
	} else if len(s.queue) < limits.ExecQueueDepth {
		qe = &queuedExec{ready: make(chan struct{}), serverName: srv.Spec.Name}
		s.queue = append(s.queue, qe)
		queuePos = len(s.queue)
	} else {
		s.execMu.Unlock()
		return nil, v1.Newf(v1.CodeConflict, "当前 server exec 并发与排队均已满（并发上限 %d，排队上限 %d）", limits.ExecMaxConcurrency, limits.ExecQueueDepth).
			WithNext("等待在途任务完成后再试，或在系统设置调大 exec_max_concurrency / exec_queue_depth")
	}
	s.execMu.Unlock()

	var releaseOnce sync.Once
	release := func() {
		releaseOnce.Do(func() {
			s.releaseExecSlot(qe, acquired)
		})
	}

	caller := v1.IdentityFrom(ctx)
	runFn := func(runCtx context.Context) (any, error) {
		return s.runExecJob(runCtx, srv, cmdText, timeout, caller, qe, queuePos)
	}

	job, err := s.deps.StartJob(ctx, srv.Metadata.ID, inv, runFn, release)
	if err != nil {
		release()
		return nil, err
	}
	if queuePos > 0 {
		job.Progress = map[string]any{
			"phase":          "queued",
			"queue_position": queuePos,
			"server":         srv.Spec.Name,
		}
	}
	return job, nil
}

func (s *Service) releaseExecSlot(qe *queuedExec, wasAcquired bool) {
	s.execMu.Lock()
	// 1. 若还在等待队列里尚未轮到，从队列中摘除并通知后方更新位置。
	if qe != nil {
		for i, item := range s.queue {
			if item == qe {
				s.queue = append(s.queue[:i], s.queue[i+1:]...)
				rest := append([]*queuedExec(nil), s.queue...)
				s.execMu.Unlock()
				for idx, r := range rest {
					r.updatePosition(idx + 1)
				}
				return
			}
		}
		// 若已不在队列里，说明已被提升获得运行槽位，需要按运行槽释放。
		wasAcquired = true
	}
	if !wasAcquired {
		s.execMu.Unlock()
		return
	}
	// 2. 运行槽释放：优先把名额直接移交队首等待者，并更新剩余队列位置。
	if len(s.queue) > 0 {
		next := s.queue[0]
		s.queue = s.queue[1:]
		rest := append([]*queuedExec(nil), s.queue...)
		s.execMu.Unlock()
		close(next.ready)
		for idx, r := range rest {
			r.updatePosition(idx + 1)
		}
		return
	}
	if s.runningCnt > 0 {
		s.runningCnt--
	}
	s.execMu.Unlock()
}

func (s *Service) registerTrack(tr *execTrack) {
	s.execMu.Lock()
	s.activeJobs[tr.jobID] = tr
	m := s.byServer[tr.serverID]
	if m == nil {
		m = map[string]*execTrack{}
		s.byServer[tr.serverID] = m
	}
	m[tr.jobID] = tr
	s.execMu.Unlock()
}

func (s *Service) unregisterTrack(tr *execTrack) {
	s.execMu.Lock()
	delete(s.activeJobs, tr.jobID)
	if m := s.byServer[tr.serverID]; m != nil {
		delete(m, tr.jobID)
		if len(m) == 0 {
			delete(s.byServer, tr.serverID)
		}
	}
	s.execMu.Unlock()
}

func (s *Service) runExecJob(
	ctx context.Context,
	srv *coreservers.Server,
	cmdText string,
	timeout time.Duration,
	caller v1.Identity,
	qe *queuedExec,
	initialQueuePos int,
) (any, error) {
	jobID := corejobs.CurrentID(ctx)
	if qe != nil {
		qe.setReporter(func(pos int) {
			corejobs.Report(ctx, map[string]any{
				"phase":          "queued",
				"queue_position": pos,
				"server":         srv.Spec.Name,
			})
		})
		corejobs.Report(ctx, map[string]any{
			"phase":          "queued",
			"queue_position": initialQueuePos,
			"server":         srv.Spec.Name,
		})
		select {
		case <-qe.ready:
			qe.setReporter(nil)
		case <-ctx.Done():
			qe.setReporter(nil)
			return nil, v1.New(v1.CodeUnavailable, "等待执行队列期间主控停止")
		}
	}

	tr := &execTrack{
		jobID:      jobID,
		serverID:   srv.Metadata.ID,
		serverName: srv.Spec.Name,
		command:    cmdText,
		actor:      caller,
		notifyCh:   make(chan struct{}, 1),
	}
	s.registerTrack(tr)
	defer s.unregisterTrack(tr)

	corejobs.Report(ctx, map[string]any{
		"phase":  "starting",
		"server": srv.Spec.Name,
	})

	startRes, err := s.deps.Nodes.CallExecStart(ctx, srv.Metadata.ID, xrpc.ExecStartParams{
		JobID:     jobID,
		Command:   cmdText,
		TimeoutMS: int(timeout / time.Millisecond),
		MaxBytes:  xrpc.ExecMaxOutputBytes,
	})
	if err != nil {
		ae := v1.AsError(err)
		s.writeExecAudit(context.WithoutCancel(ctx), caller, jobID, srv.Spec.Name, cmdText, ae.Reason, string(ae.Code), nil, false)
		return nil, err
	}

	corejobs.Report(ctx, map[string]any{
		"phase":        "running",
		"server":       srv.Spec.Name,
		"pgid":         startRes.PGID,
		"output_bytes": 0,
	})

	pollEvery := s.deps.PollInterval
	if s.deps.Nodes.ConnectedWS(srv.Metadata.ID) {
		pollEvery = s.deps.WSFallbackPoll
	}
	ticker := time.NewTicker(pollEvery)
	defer ticker.Stop()

	safetyTimer := time.NewTimer(timeout + 15*time.Second)
	defer safetyTimer.Stop()

	reportRunning := func() {
		tr.mu.Lock()
		out := tr.buf.String()
		trunc := tr.truncated
		tr.mu.Unlock()
		preview := out
		if len(preview) > execPreviewMaxBytes {
			preview = preview[len(preview)-execPreviewMaxBytes:]
		}
		corejobs.Report(ctx, map[string]any{
			"phase":          "running",
			"server":         srv.Spec.Name,
			"pgid":           startRes.PGID,
			"output_bytes":   len(out),
			"output_preview": preview,
			"truncated":      trunc,
		})
	}

	finalizeUnknown := func(reason string) (any, error) {
		tr.mu.Lock()
		partial := tr.buf.String()
		trunc := tr.truncated
		tr.mu.Unlock()
		res := ExecJobResult{
			Server:        srv.Spec.Name,
			ServerID:      srv.Metadata.ID,
			Command:       cmdText,
			State:         corejobs.StatusUnknown,
			Output:        partial,
			Truncated:     trunc,
			UnknownReason: reason,
		}
		raw, _ := json.Marshal(res)
		s.writeExecAudit(context.WithoutCancel(ctx), caller, jobID, srv.Spec.Name, cmdText, partial, corejobs.StatusUnknown, nil, trunc)
		return corejobs.Outcome{
			Status:          corejobs.StatusUnknown,
			ExitCode:        nil,
			Output:          string(raw),
			OutputTruncated: trunc,
		}, nil
	}

	finalizeDone := func() (any, error) {
		tr.mu.Lock()
		full := tr.buf.String()
		trunc := tr.truncated
		state := tr.state
		code := tr.exitCode
		durMS := tr.durMS
		errMsg := tr.errMsg
		tr.mu.Unlock()

		exitCopy := code
		res := ExecJobResult{
			Server:     srv.Spec.Name,
			ServerID:   srv.Metadata.ID,
			Command:    cmdText,
			State:      state,
			ExitCode:   &exitCopy,
			Output:     full,
			Truncated:  trunc,
			DurationMS: durMS,
			Error:      errMsg,
		}
		raw, _ := json.Marshal(res)
		jobStatus := corejobs.StatusFailed
		auditResult := "failed"
		if state == xrpc.ExecStateDone && code == 0 {
			jobStatus = corejobs.StatusDone
			auditResult = "ok"
		}
		exit64 := int64(code)
		s.writeExecAudit(context.WithoutCancel(ctx), caller, jobID, srv.Spec.Name, cmdText, full, auditResult, &code, trunc)
		return corejobs.Outcome{
			Status:          jobStatus,
			ExitCode:        &exit64,
			Output:          string(raw),
			OutputTruncated: trunc,
		}, nil
	}

	for {
		tr.mu.Lock()
		done := tr.done
		offline := tr.offline
		curOffset := tr.buf.Len()
		tr.mu.Unlock()

		if done {
			return finalizeDone()
		}
		if offline {
			return finalizeUnknown("执行期间与节点的连接断开，命令可能仍在节点继续运行或已结束；结果未知不等于可重试，待节点重连后主控将自动按 job_id 补报终态")
		}

		select {
		case <-ctx.Done():
			cctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			_, _ = s.deps.Nodes.CallExecCancel(cctx, srv.Metadata.ID, xrpc.ExecCancelParams{JobID: jobID})
			cancel()
			return finalizeUnknown("主控停止时远程命令仍在执行，已尽力发送取消指令，结果未知")
		case <-tr.notifyCh:
			reportRunning()
		case <-safetyTimer.C:
			return finalizeUnknown("等待节点回报命令终态超时，结果未知；切勿盲目重试，请待节点状态恢复后核查")
		case <-ticker.C:
			st, serr := s.deps.Nodes.CallExecStatus(ctx, srv.Metadata.ID, xrpc.ExecStatusParams{
				JobID:  jobID,
				Offset: curOffset,
			})
			if serr != nil {
				if v1.AsError(serr).Code == v1.CodeUnavailable {
					return finalizeUnknown("轮询节点命令状态时连接不可达，命令可能仍在运行；结果未知不等于可重试，待节点恢复后将按 job_id 补报终态")
				}
				continue
			}
			if !st.Found {
				return finalizeUnknown("节点未找到在途任务状态（可能已重启），结果未知")
			}
			if st.Done && st.Output != "" {
				tr.replaceFullOutput(st.Output, st.Truncated)
			} else if st.Chunk != "" || st.Truncated {
				startOff := st.NextOffset - len(st.Chunk)
				tr.appendFromOffset(startOff, st.Chunk, st.Truncated)
			}
			if st.Done {
				tr.mu.Lock()
				tr.done = true
				tr.state = st.State
				tr.exitCode = st.ExitCode
				tr.durMS = st.DurationMS
				tr.errMsg = st.Error
				tr.mu.Unlock()
				return finalizeDone()
			}
			reportRunning()
		}
	}
}

// HandleExecEvent 处理节点经 WebSocket 上推的 exec_event 增量输出或终态帧（由 agentconn.OnExecEvent 接线）。
func (s *Service) HandleExecEvent(serverID int64, ev xrpc.ExecEvent) {
	if ev.JobID == "" {
		return
	}
	s.execMu.Lock()
	tr := s.activeJobs[ev.JobID]
	s.execMu.Unlock()
	if tr == nil || tr.serverID != serverID {
		return
	}
	if ev.Chunk != "" || ev.Truncated {
		startOff := ev.Offset - len(ev.Chunk)
		tr.appendFromOffset(startOff, ev.Chunk, ev.Truncated)
	}
	if ev.Done {
		tr.mu.Lock()
		tr.done = true
		tr.state = ev.State
		tr.exitCode = ev.ExitCode
		tr.durMS = ev.DurationMS
		tr.errMsg = ev.Error
		tr.mu.Unlock()
	}
	tr.signal()
}

// HandleNodeOffline 在节点连接断开时将在途 server exec 立即唤醒并标为 unknown（绝不误标为 failed）。
func (s *Service) HandleNodeOffline(serverID int64) {
	if serverID <= 0 {
		return
	}
	s.execMu.Lock()
	s.recentOffline[serverID] = s.now()
	tracks := make([]*execTrack, 0, len(s.byServer[serverID]))
	for _, tr := range s.byServer[serverID] {
		tracks = append(tracks, tr)
	}
	s.execMu.Unlock()

	for _, tr := range tracks {
		tr.mu.Lock()
		if !tr.done {
			tr.offline = true
		}
		tr.mu.Unlock()
		tr.signal()
	}
}

// HandleNodeConnected 在节点重连上线后按 job_id 向节点调用 exec.status，将遗留的 unknown 任务补报收敛到终态。
func (s *Service) HandleNodeConnected(serverID int64) {
	if serverID <= 0 || s.deps.ListUnknownJobs == nil || s.deps.ReconcileUnknown == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	s.reconcileServerUnknown(ctx, serverID)

	// 若刚发生短时断连重连，200ms 后复检一次，兜住断连瞬间写库与重连的竞态窗口。
	s.execMu.Lock()
	lastOff, hadOff := s.recentOffline[serverID]
	s.execMu.Unlock()
	if hadOff && s.now().Sub(lastOff) <= execReconnectGrace {
		time.Sleep(200 * time.Millisecond)
		rctx, rcancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer rcancel()
		s.reconcileServerUnknown(rctx, serverID)
	}
}

func (s *Service) reconcileServerUnknown(ctx context.Context, serverID int64) {
	jobs, err := s.deps.ListUnknownJobs(ctx, serverID, execJobKind)
	if err != nil || len(jobs) == 0 {
		return
	}
	for _, j := range jobs {
		s.tryReconcileOne(ctx, serverID, j)
	}
}

func (s *Service) tryReconcileOne(ctx context.Context, serverID int64, j *corejobs.Job) {
	s.execMu.Lock()
	if s.reconciling[j.JobID] {
		s.execMu.Unlock()
		return
	}
	s.reconciling[j.JobID] = true
	s.execMu.Unlock()

	st, err := s.deps.Nodes.CallExecStatus(ctx, serverID, xrpc.ExecStatusParams{
		JobID:  j.JobID,
		Offset: 0,
	})
	if err != nil || !st.Found {
		s.execMu.Lock()
		delete(s.reconciling, j.JobID)
		s.execMu.Unlock()
		return
	}
	if st.Done {
		s.applyReconciledTerminal(ctx, serverID, j, st)
		s.execMu.Lock()
		delete(s.reconciling, j.JobID)
		s.execMu.Unlock()
		return
	}
	// 命令在节点侧仍在继续执行：后台短轮询直至收敛或再次断连。
	go func() {
		defer func() {
			s.execMu.Lock()
			delete(s.reconciling, j.JobID)
			s.execMu.Unlock()
		}()
		wctx, cancel := context.WithTimeout(context.Background(), execReconcileTimeout)
		defer cancel()
		ticker := time.NewTicker(300 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-wctx.Done():
				return
			case <-ticker.C:
				cur, cerr := s.deps.Nodes.CallExecStatus(wctx, serverID, xrpc.ExecStatusParams{
					JobID:  j.JobID,
					Offset: 0,
				})
				if cerr != nil || !cur.Found {
					return
				}
				if cur.Done {
					s.applyReconciledTerminal(wctx, serverID, j, cur)
					return
				}
			}
		}
	}()
}

func (s *Service) applyReconciledTerminal(ctx context.Context, serverID int64, j *corejobs.Job, st *xrpc.ExecStatusResult) {
	var prev ExecJobResult
	if j.Output != nil && *j.Output != "" {
		_ = json.Unmarshal([]byte(*j.Output), &prev)
	}
	srvName := prev.Server
	if srvName == "" && s.deps.Servers != nil {
		if srv, err := s.deps.Servers.ByID(ctx, serverID); err == nil {
			srvName = srv.Spec.Name
		}
	}
	fullOut := st.Output
	if fullOut == "" && prev.Output != "" {
		fullOut = prev.Output
	}
	exitCopy := st.ExitCode
	finalRes := ExecJobResult{
		Server:     srvName,
		ServerID:   serverID,
		Command:    prev.Command,
		State:      st.State,
		ExitCode:   &exitCopy,
		Output:     fullOut,
		Truncated:  st.Truncated,
		DurationMS: st.DurationMS,
		Error:      st.Error,
		Reconciled: true,
	}
	raw, _ := json.Marshal(finalRes)
	jobStatus := corejobs.StatusFailed
	auditResult := "failed"
	if st.State == xrpc.ExecStateDone && st.ExitCode == 0 {
		jobStatus = corejobs.StatusDone
		auditResult = "ok"
	}
	if err := s.deps.ReconcileUnknown(ctx, j.JobID, jobStatus, int64(st.ExitCode), string(raw), st.Truncated); err != nil {
		if !errors.Is(err, corejobs.ErrNotFound) {
			s.deps.Logger.Warn("补报 unknown exec 任务终态失败", "job_id", j.JobID, "err", err)
		}
		return
	}
	actor := identityFromJobArgs(j.Args)
	s.writeExecAudit(ctx, actor, j.JobID, srvName, prev.Command, fullOut, auditResult, &exitCopy, st.Truncated)
}

func identityFromJobArgs(raw json.RawMessage) v1.Identity {
	var parsed struct {
		Actor     string       `json:"actor"`
		ActorKind v1.ActorKind `json:"actor_kind"`
	}
	if len(raw) > 0 && json.Unmarshal(raw, &parsed) == nil && parsed.Actor != "" {
		return v1.Identity{Actor: parsed.Actor, ActorKind: parsed.ActorKind, Role: v1.RoleAdmin}
	}
	return v1.Identity{Actor: "system", ActorKind: v1.ActorSystem, Role: v1.RoleAdmin}
}

func (s *Service) writeExecAudit(
	ctx context.Context,
	actor v1.Identity,
	jobID, serverName, fullCommand, rawOutput, result string,
	exitCode *int,
	truncated bool,
) {
	if s.deps.AuditRepo == nil {
		return
	}
	redacted := RedactSensitiveText(rawOutput)
	if len(redacted) > xrpc.ExecMaxOutputBytes {
		redacted = redacted[:xrpc.ExecMaxOutputBytes]
		truncated = true
	}
	digestBytes, _ := json.Marshal(map[string]any{
		"job_id":           jobID,
		"server":           serverName,
		"exit_code":        exitCode,
		"output_truncated": truncated,
		"phase":            "exec_result",
	})
	fc := fullCommand
	out := redacted
	rec := coreaudit.Record{
		At:          s.now(),
		Actor:       actor.Actor,
		ActorKind:   actor.ActorKind,
		TokenID:     actor.TokenID,
		Command:     execJobKind,
		ArgsDigest:  string(digestBytes),
		FullCommand: &fc,
		Output:      &out,
		Result:      result,
	}
	if err := s.deps.AuditRepo.Insert(ctx, rec); err != nil {
		s.deps.Logger.Warn("写入 server exec 执行审计失败", "job_id", jobID, "err", err)
	}
}
