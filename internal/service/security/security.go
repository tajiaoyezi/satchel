// Package security 是业务层的令牌猜测防护与安全事件（master-login-protection）：按来源 IP 数令牌校验失败，
// 达到上限自动封禁；手动封禁与解封；封禁在内存里查、在 ip_bans 里存；以及 security events list / bans list /
// ban / unban 四条命令。封禁只拦带令牌的请求（门在判定身份之前问 Banned），猜密码由 service/auth 的登录限流管。
package security

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/satchel/satchel/internal/base/ipaddr"
	"github.com/satchel/satchel/internal/command"
	core "github.com/satchel/satchel/internal/core/security"
	v1 "github.com/satchel/satchel/pkg/api/v1"
)

// sweepInterval 是清理内存里过期计数与封禁的间隔（照 mmwx）。
const sweepInterval = 10 * time.Minute

// Config 是令牌猜测防护的参数，来自系统设置门这一组（brute_force_* 与 skip_local_ip）。
type Config struct {
	Enabled     bool
	MaxFailures int
	Window      time.Duration
	Block       time.Duration
	SkipLocalIP bool
}

// DefaultConfig 是设置里没有值时的参数（照 mmwx：24 小时内 5 次、封 24 小时、跳过本地与内网地址）。
func DefaultConfig() Config {
	return Config{Enabled: true, MaxFailures: 5, Window: 24 * time.Hour, Block: 24 * time.Hour, SkipLocalIP: true}
}

type probeCount struct {
	count int
	first time.Time
}

// Service 持有仓储与两张内存表：每个 IP 的失败计数、生效中的封禁（ip_bans 是它的持久副本）。
type Service struct {
	repo   *core.Repo
	logger *slog.Logger

	mu     sync.Mutex
	now    func() time.Time
	cfg    Config
	probes map[string]*probeCount
	bans   map[string]core.Ban
}

// New 建服务；logger 为 nil 时用 slog.Default()。参数先用默认值，装配根在启动与每次设置写之后调 Configure。
func New(repo *core.Repo, logger *slog.Logger) *Service {
	if logger == nil {
		logger = slog.Default()
	}
	return &Service{repo: repo, logger: logger, now: func() time.Time { return time.Now().UTC() }, cfg: DefaultConfig(),
		probes: map[string]*probeCount{}, bans: map[string]core.Ban{}}
}

// SetNow 换掉时钟（测试把时间往后拨，不用真的等）。
func (s *Service) SetNow(now func() time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.now = now
}

// Configure 换上新的参数，下一次判定就按它算；已有的计数与封禁不动。
func (s *Service) Configure(c Config) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cfg = c
}

func (s *Service) config() (Config, time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cfg, s.now()
}

// Bindings 是本服务提供的命令处理函数，按命令名给 cmd/satchel 绑定。
func (s *Service) Bindings() command.Bindings {
	return command.Bindings{
		"security events list": s.eventsList,
		"security bans list":   s.bansList,
		"security ban":         s.ban,
		"security unban":       s.unban,
	}
}

// RecordProbe 记一次令牌校验失败（authn 在判定出无效凭据、且请求不经 unix socket 时调）。关着开关、IP 为空、
// skip_local_ip 下的本地与内网地址、已在封禁里的 IP 都不计。达到上限的那一次自动封禁：写 ip_bans、记 ban 事件、清掉计数；
// 没到上限记 probe 事件。写库失败只记日志，不影响请求。
func (s *Service) RecordProbe(ctx context.Context, ip, path string) {
	s.mu.Lock()
	cfg, now := s.cfg, s.now()
	if !cfg.Enabled || ip == "" || (cfg.SkipLocalIP && ipaddr.IsLocalOrPrivate(ip)) {
		s.mu.Unlock()
		return
	}
	if b, ok := s.bans[ip]; ok && b.Active(now) {
		s.mu.Unlock()
		return
	}
	p := s.probes[ip]
	if p == nil || now.Sub(p.first) > cfg.Window {
		p = &probeCount{first: now}
		s.probes[ip] = p
	}
	p.count++
	count := p.count
	var ban *core.Ban
	if count >= cfg.MaxFailures {
		delete(s.probes, ip)
		until := now.Add(cfg.Block)
		ban = &core.Ban{IP: ip, Reason: core.ReasonBruteForce, BannedAt: now, ExpiresAt: &until, FailCount: int64(count)}
		s.bans[ip] = *ban
	}
	s.mu.Unlock()

	ctx = context.WithoutCancel(ctx)
	if ban == nil {
		s.record(ctx, core.Event{At: now, IP: ip, Kind: core.KindProbe, Path: path, Detail: fmt.Sprintf("%d/%d", count, cfg.MaxFailures)})
		return
	}
	if err := s.repo.UpsertBan(ctx, *ban); err != nil {
		s.logger.Error("自动封禁写库失败，本进程内照样生效，重启后不再恢复", "ip", ip, "error", err)
	}
	s.logger.Warn("来源 IP 令牌校验失败次数达到上限，已自动封禁", "ip", ip, "fail_count", count, "until", ban.ExpiresAt)
	s.record(ctx, core.Event{At: now, IP: ip, Kind: core.KindBan, Path: path, Detail: fmt.Sprintf("fail=%d", count)})
}

// Banned 报告 ip 是否在生效中的封禁里（门在判定身份之前对带 Authorization 头的请求问它）。只查内存；
// skip_local_ip 开着时本地与内网地址一律不算被封；到期的封禁顺手从内存里删掉。
func (s *Service) Banned(ip string) (core.Ban, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ip == "" || (s.cfg.SkipLocalIP && ipaddr.IsLocalOrPrivate(ip)) {
		return core.Ban{}, false
	}
	b, ok := s.bans[ip]
	if !ok {
		return core.Ban{}, false
	}
	if !b.Active(s.now()) {
		delete(s.bans, ip)
		return core.Ban{}, false
	}
	return b, true
}

// Restore 在启动时把 ip_bans 里生效中的封禁灌回内存：重启不解封。
func (s *Service) Restore(ctx context.Context) error {
	_, now := s.config()
	bans, err := s.repo.ActiveBans(ctx, now)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, b := range bans {
		s.bans[b.IP] = b
	}
	if len(bans) > 0 {
		s.logger.Info("已从库里恢复 IP 封禁", "count", len(bans))
	}
	return nil
}

// Run 每 10 分钟清一次内存里到期的封禁与过了窗口的计数，直到 ctx 取消（m1-06 的定时任务接上之后改挂到那里）。
func (s *Service) Run(ctx context.Context) {
	t := time.NewTicker(sweepInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.sweep()
		}
	}
}

func (s *Service) sweep() {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	for ip, b := range s.bans {
		if !b.Active(now) {
			delete(s.bans, ip)
		}
	}
	for ip, p := range s.probes {
		if now.Sub(p.first) > s.cfg.Window {
			delete(s.probes, ip)
		}
	}
}

// record 写一条安全事件；失败只记 error 日志（安全事件是给人看的线索，不能反过来让请求失败）。
func (s *Service) record(ctx context.Context, e core.Event) {
	if err := s.repo.InsertEvent(ctx, e); err != nil {
		s.logger.Error("写安全事件失败", "kind", e.Kind, "ip", e.IP, "path", e.Path, "username", e.Username, "detail", e.Detail, "actor", e.Actor, "error", err)
	}
}

func requireAdmin(ctx context.Context) error {
	if !v1.IdentityFrom(ctx).IsAdmin() {
		return v1.New(v1.CodeForbidden, "安全事件与 IP 封禁只对管理员开放")
	}
	return nil
}

func canonicalIP(raw string) (string, error) {
	ip, ok := ipaddr.Canonical(raw)
	if !ok {
		return "", v1.Newf(v1.CodeBadRequest, "%q 不是单个 IP 地址", raw).WithNext("写成 198.51.100.7 或 2001:db8::1 这样的形状，不支持网段")
	}
	return ip, nil
}

func (s *Service) eventsList(ctx context.Context, inv *command.Invocation) (any, error) {
	if err := requireAdmin(ctx); err != nil {
		return nil, err
	}
	page := command.Page{}
	if inv.Page != nil {
		page = *inv.Page
	}
	if err := page.Normalize(); err != nil {
		return nil, err
	}
	beforeID, err := command.DecodeIDCursor(page.Cursor)
	if err != nil {
		return nil, err
	}
	f := core.EventFilter{Kind: inv.String("kind", ""), IP: inv.String("ip", "")}
	total, err := s.repo.CountEvents(ctx, f)
	if err != nil {
		return nil, err
	}
	// 多取一条，用来判断有没有下一页。
	rows, err := s.repo.ListEvents(ctx, f, page.Limit+1, beforeID)
	if err != nil {
		return nil, err
	}
	res := &command.PageResult{Items: make([]any, 0, len(rows)), Total: total}
	if len(rows) > page.Limit {
		rows = rows[:page.Limit]
		res.NextCursor = command.EncodeIDCursor(rows[len(rows)-1].ID)
	}
	for _, r := range rows {
		res.Items = append(res.Items, r)
	}
	return res, nil
}

func (s *Service) bansList(ctx context.Context, inv *command.Invocation) (any, error) {
	if err := requireAdmin(ctx); err != nil {
		return nil, err
	}
	page := command.Page{}
	if inv.Page != nil {
		page = *inv.Page
	}
	if err := page.Normalize(); err != nil {
		return nil, err
	}
	offset, err := command.DecodeOffsetCursor(page.Cursor)
	if err != nil {
		return nil, err
	}
	_, now := s.config()
	total, err := s.repo.CountActiveBans(ctx, now)
	if err != nil {
		return nil, err
	}
	rows, err := s.repo.ListActiveBans(ctx, now, page.Limit+1, offset)
	if err != nil {
		return nil, err
	}
	res := &command.PageResult{Items: make([]any, 0, len(rows)), Total: total}
	if len(rows) > page.Limit {
		rows = rows[:page.Limit]
		res.NextCursor = command.EncodeOffsetCursor(offset + page.Limit)
	}
	for _, r := range rows {
		res.Items = append(res.Items, r)
	}
	return res, nil
}

// ban 是 security ban：人类专属（当场验证由 authz 在这之前做完），只对管理员开放。先写库再改内存，写库失败内存不动。
func (s *Service) ban(ctx context.Context, inv *command.Invocation) (any, error) {
	if err := requireAdmin(ctx); err != nil {
		return nil, err
	}
	ip, err := canonicalIP(inv.Arg(0))
	if err != nil {
		return nil, err
	}
	cfg, now := s.config()
	if cfg.SkipLocalIP && ipaddr.IsLocalOrPrivate(ip) {
		return nil, v1.Newf(v1.CodeBadRequest, "%s 是本地或内网地址，skip_local_ip 开着时这样的地址不受封禁，这条封禁不会生效", ip).
			WithNext("确实要封，先用 satchel settings gates set --set skip_local_ip=false 关掉这个开关")
	}
	permanent := inv.Bool("permanent")
	actor := v1.IdentityFrom(ctx).Actor
	b := core.Ban{IP: ip, Reason: core.ReasonManual, BannedAt: now, Permanent: permanent, Actor: actor}
	detail := "permanent"
	if !permanent {
		until := now.Add(cfg.Block)
		b.ExpiresAt = &until
		detail = "until=" + until.Format(time.RFC3339)
	}
	if err := s.repo.UpsertBan(ctx, b); err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.bans[ip] = b
	delete(s.probes, ip)
	s.mu.Unlock()
	s.record(context.WithoutCancel(ctx), core.Event{At: now, IP: ip, Kind: core.KindBanManual, Detail: detail, Actor: actor})
	return b, nil
}

// unban 是 security unban：把生效中的封禁标为已解封（库里留痕、不删行）并从内存里去掉；两边都没有才是 not_found。
func (s *Service) unban(ctx context.Context, inv *command.Invocation) (any, error) {
	if err := requireAdmin(ctx); err != nil {
		return nil, err
	}
	ip, err := canonicalIP(inv.Arg(0))
	if err != nil {
		return nil, err
	}
	_, now := s.config()
	actor := v1.IdentityFrom(ctx).Actor
	released, err := s.repo.ReleaseBan(ctx, ip, actor, now)
	if err != nil && !errors.Is(err, core.ErrNotFound) {
		return nil, err
	}
	s.mu.Lock()
	inMemory, had := s.bans[ip]
	delete(s.bans, ip)
	s.mu.Unlock()
	if released == nil {
		// 库里没有生效中的封禁：只剩内存里的（自动封禁写库失败时）才算有。
		if !had || !inMemory.Active(now) {
			return nil, v1.Newf(v1.CodeNotFound, "%s 没有生效中的封禁", ip).WithNext("看生效中的封禁：satchel security bans list")
		}
		inMemory.ReleasedAt, inMemory.Actor = &now, actor
		released = &inMemory
	}
	s.record(context.WithoutCancel(ctx), core.Event{At: now, IP: ip, Kind: core.KindUnban, Actor: actor})
	return *released, nil
}
