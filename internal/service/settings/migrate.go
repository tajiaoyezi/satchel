package settings

import (
	"context"
	"net/url"
	"strings"

	"github.com/satchel/satchel/internal/command"
	core "github.com/satchel/satchel/internal/core/settings"
	v1 "github.com/satchel/satchel/pkg/api/v1"
)

// MigrationNodeProbe 记录节点对候选 master_url 的预检与迁移可达性结果。
type MigrationNodeProbe struct {
	Server    string `json:"server"`
	DBStatus  string `json:"db_status,omitempty"`
	State     string `json:"state"` // ready | unreachable | offline
	LatencyMS int64  `json:"latency_ms,omitempty"`
	SameHost  bool   `json:"same_host_as_master"`
	Pending   bool   `json:"pending,omitempty"`
	Error     string `json:"error,omitempty"`
}

// MigrationHooks 是装配根注入的节点探测与推送回调，保持 settings 与 servers 业务层解耦。
type MigrationHooks interface {
	ProbeNodes(ctx context.Context, candidateURL string) ([]MigrationNodeProbe, error)
	PushNodes(ctx context.Context, targetURL string, servers []string) error
}

// SetMigration 注入主控地址迁移时向节点探测与推送新地址的钩子。
func (s *Service) SetMigration(h MigrationHooks) { s.migration = h }

// MasterURLMigrateOutput 是 settings master-url migrate（含 --preview）的结构化输出。
type MasterURLMigrateOutput struct {
	Preview          bool                 `json:"preview"`
	MasterURL        string               `json:"master_url"`
	SubscriptionURL  string               `json:"subscription_url,omitempty"`
	TurnstileCleared bool                 `json:"turnstile_cleared"`
	CanMigrate       bool                 `json:"can_migrate,omitempty"`
	Ready            int                  `json:"ready"`
	Unreachable      int                  `json:"unreachable"`
	Offline          int                  `json:"offline"`
	Pushed           []string             `json:"pushed,omitempty"`
	SkippedOffline   []string             `json:"skipped_offline,omitempty"`
	Blocking         []MigrationNodeProbe `json:"blocking,omitempty"`
	Nodes            []MigrationNodeProbe `json:"nodes"`
	Settings         any                  `json:"settings,omitempty"`
}

func (s *Service) probeMigrationNodes(ctx context.Context, candidateURL string) ([]MigrationNodeProbe, error) {
	if s.migration == nil {
		return []MigrationNodeProbe{}, nil
	}
	return s.migration.ProbeNodes(ctx, candidateURL)
}

func classifyMigrationNodes(nodes []MigrationNodeProbe, skipUnreachable bool) (readyNames, skippedOffline []string, blocking []MigrationNodeProbe, readyCnt, unreachableCnt, offlineCnt int) {
	for _, n := range nodes {
		switch n.State {
		case "ready":
			readyCnt++
			readyNames = append(readyNames, n.Server)
		case "offline":
			offlineCnt++
			if n.Pending || skipUnreachable {
				skippedOffline = append(skippedOffline, n.Server)
			} else {
				blocking = append(blocking, n)
			}
		default:
			unreachableCnt++
			blocking = append(blocking, n)
		}
	}
	return
}

func shouldClearTurnstile(oldMasterURL, newMasterURL string, changeDomain bool) bool {
	if changeDomain {
		return true
	}
	oldU, oerr := url.Parse(strings.TrimSpace(oldMasterURL))
	newU, nerr := url.Parse(strings.TrimSpace(newMasterURL))
	if oerr != nil || nerr != nil || oldU.Host == "" {
		return false
	}
	return !strings.EqualFold(oldU.Scheme+"://"+oldU.Host, newU.Scheme+"://"+newU.Host)
}

func (s *Service) masterURLMigrate(ctx context.Context, inv *command.Invocation) (any, error) {
	if err := requireAdmin(ctx); err != nil {
		return nil, err
	}
	rawURL, givenURL := inv.Flags["url"]
	if !givenURL {
		return nil, v1.New(v1.CodeBadRequest, "必须提供 --url 参数指定目标 https:// 主控地址")
	}
	strURL, _ := rawURL.(string)
	targetOrigin, err := normalizeOrigin("url", strURL)
	if err != nil {
		return nil, err
	}
	if targetOrigin == "" || !strings.HasPrefix(targetOrigin, "https://") {
		return nil, v1.Newf(v1.CodeBadRequest, "主控迁移要求干净的 https:// 地址，得到 %q", strURL).
			WithNext("例如 https://new.example.com")
	}

	var (
		hasSub    bool
		subOrigin string
	)
	if rawSub, givenSub := inv.Flags["subscription-url"]; givenSub {
		hasSub = true
		strSub, _ := rawSub.(string)
		subOrigin, err = normalizeOrigin("subscription-url", strSub)
		if err != nil {
			return nil, err
		}
	}

	preview := inv.Bool("preview")
	changeDomain := inv.Bool("change-domain")
	skipUnreachable := inv.Bool("skip-unreachable")

	cur, err := s.repo.Load(ctx)
	if err != nil {
		return nil, err
	}
	willClearTurnstile := shouldClearTurnstile(cur.Gates().MasterURL, targetOrigin, changeDomain)

	if preview {
		nodes, err := s.probeMigrationNodes(ctx, targetOrigin)
		if err != nil {
			return nil, err
		}
		_, skippedOffline, blocking, readyCnt, unreachableCnt, offlineCnt := classifyMigrationNodes(nodes, skipUnreachable)
		return &MasterURLMigrateOutput{
			Preview:          true,
			MasterURL:        targetOrigin,
			SubscriptionURL:  subOrigin,
			TurnstileCleared: willClearTurnstile,
			CanMigrate:       len(blocking) == 0,
			Ready:            readyCnt,
			Unreachable:      unreachableCnt,
			Offline:          offlineCnt,
			SkippedOffline:   skippedOffline,
			Blocking:         blocking,
			Nodes:            nodes,
		}, nil
	}

	expected, err := versionArgs(inv)
	if err != nil {
		return nil, err
	}
	if cur.Version != expected {
		return nil, v1.Newf(v1.CodeVersionConflict, "SystemSettings 期望版本是 %d，当前版本是 %d", expected, cur.Version).
			WithState("expected", expected).WithState("current", cur.Version)
	}

	nodes, err := s.probeMigrationNodes(ctx, targetOrigin)
	if err != nil {
		return nil, err
	}
	readyNames, skippedOffline, blocking, readyCnt, unreachableCnt, offlineCnt := classifyMigrationNodes(nodes, skipUnreachable)
	if len(blocking) > 0 {
		names := make([]string, 0, len(blocking))
		for _, b := range blocking {
			names = append(names, b.Server)
		}
		return nil, v1.Newf(v1.CodeConflict, "主控地址迁移被 %d 台阻塞节点中止：%s", len(blocking), strings.Join(names, ", ")).
			WithNext("修复节点到候选地址的连通性，或对确已离线节点显式加 --skip-unreachable（节点重连后会自动收敛）").
			WithState("blocking", blocking).
			WithState("nodes", nodes)
	}

	values := map[string]any{"master_url": targetOrigin}
	if hasSub {
		values["subscription_url"] = subOrigin
	}
	if willClearTurnstile {
		values["turnstile_site_key"] = ""
		values["turnstile_secret_key"] = ""
	}
	prepared, err := s.prepare(values, humanOnly, inv.Name())
	if err != nil {
		return nil, err
	}

	if s.migration != nil && len(readyNames) > 0 {
		if err := s.migration.PushNodes(ctx, targetOrigin, readyNames); err != nil {
			return nil, err
		}
	}

	st, err := s.repo.Write(ctx, core.WriteRequest{Values: prepared, ExpectedVersion: expected})
	if err != nil {
		return nil, err
	}
	s.written(st)
	obj, err := s.output(ctx, st)
	if err != nil {
		return nil, err
	}
	return &MasterURLMigrateOutput{
		Preview:          false,
		MasterURL:        targetOrigin,
		SubscriptionURL:  subOrigin,
		TurnstileCleared: willClearTurnstile,
		CanMigrate:       true,
		Ready:            readyCnt,
		Unreachable:      unreachableCnt,
		Offline:          offlineCnt,
		Pushed:           readyNames,
		SkippedOffline:   skippedOffline,
		Nodes:            nodes,
		Settings:         obj,
	}, nil
}
