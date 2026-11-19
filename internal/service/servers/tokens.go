package servers

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/satchel/satchel/internal/base/agentconn"
	"github.com/satchel/satchel/internal/command"
	core "github.com/satchel/satchel/internal/core/servers"
	v1 "github.com/satchel/satchel/pkg/api/v1"
	"github.com/satchel/satchel/pkg/xrpc"
)

type tokenController interface {
	BeginTokenWindow(serverID int64, node agentconn.Node, oldToken string)
	ConfirmTokenWindow(serverID int64)
	InvalidateServer(serverID int64, dropUplink bool, roles ...string)
}

type agentTokenController interface {
	BeginAgentTokenWindow(serverID int64, oldAgentToken string)
	ConfirmAgentTokenWindow(serverID int64)
}

// TokenView 是 server token show 返回的三把凭据原文与轮换/吊销待确认状态。
type TokenView struct {
	Server          string     `json:"server"`
	Token           string     `json:"token"`
	AgentToken      string     `json:"agent_token"`
	PullToken       string     `json:"pull_token"`
	RotationPending bool       `json:"rotation_pending"`
	LastRotatedAt   *time.Time `json:"last_rotated_at"`
	RevokePending   bool       `json:"revoke_pending"`
}

func parseTokenScope(raw string) (doToken, doAgent, doPull bool, roles []string, err error) {
	switch strings.TrimSpace(raw) {
	case "", "all":
		return true, true, true, []string{"manage", "pull"}, nil
	case "token":
		return true, false, false, nil, nil
	case "agent":
		return false, true, false, []string{"manage"}, nil
	case "pull":
		return false, false, true, []string{"pull"}, nil
	default:
		return false, false, false, nil, v1.Newf(v1.CodeBadRequest, "--which %q 不合法：必须是 all、token、agent 或 pull 之一", raw)
	}
}

func (s *Service) tokenShow(ctx context.Context, inv *command.Invocation) (any, error) {
	if err := requireAdmin(ctx); err != nil {
		return nil, err
	}
	if !v1.IdentityFrom(ctx).HasScope(v1.ScopeSecrets) {
		return nil, v1.New(v1.CodeForbidden, "查看服务器凭据原文需要密钥读取（secrets）权限范围").
			WithNext("在主控本机执行 sudo satchel server token show <名字>，或使用已授予 secrets 权限的令牌")
	}
	srv, err := s.byName(ctx, inv.Arg(0))
	if err != nil {
		return nil, err
	}
	return &TokenView{
		Server:          srv.Spec.Name,
		Token:           srv.Status.Token.Reveal(),
		AgentToken:      srv.Status.AgentToken.Reveal(),
		PullToken:       srv.Status.PullToken.Reveal(),
		RotationPending: srv.Status.RotationPending,
		LastRotatedAt:   srv.Status.LastRotatedAt,
		RevokePending:   srv.Status.RevokePending,
	}, nil
}

func (s *Service) tokenRotate(ctx context.Context, inv *command.Invocation) (any, error) {
	if err := requireAdmin(ctx); err != nil {
		return nil, err
	}
	doTok, doAgent, doPull, roles, err := parseTokenScope(inv.String("which", "all"))
	if err != nil {
		return nil, err
	}
	prev, err := s.byName(ctx, inv.Arg(0))
	if err != nil {
		return nil, err
	}
	fresh, err := core.NewTokens(3)
	if err != nil {
		return nil, v1.Wrap(v1.CodeInternal, "生成新凭据失败", err)
	}
	var (
		patch core.TokensPatch
		rpc   xrpc.TokenRotateParams
	)
	if doTok {
		patch.Token = &fresh[0]
		rpc.Token = fresh[0]
	}
	if doAgent {
		patch.AgentToken = &fresh[1]
		rpc.AgentToken = fresh[1]
	}
	if doPull {
		patch.PullToken = &fresh[2]
		rpc.PullToken = fresh[2]
	}

	conns := s.getConns()
	if conns == nil {
		return nil, nodeError(prev.Spec.Name, xrpc.MethodTokenRotate, callTimeout, agentconn.ErrOffline)
	}

	var ctrl tokenController
	if tc, ok := conns.(tokenController); ok {
		ctrl = tc
	}
	var agentCtrl agentTokenController
	if ac, ok := conns.(agentTokenController); ok {
		agentCtrl = ac
	}
	if doTok && ctrl != nil {
		ctrl.BeginTokenWindow(prev.Metadata.ID, agentconn.Node{ID: prev.Metadata.ID, Name: prev.Spec.Name}, prev.Status.Token.Reveal())
	}
	if doAgent && agentCtrl != nil {
		agentCtrl.BeginAgentTokenWindow(prev.Metadata.ID, prev.Status.AgentToken.Reveal())
	}

	now := time.Now().UTC()
	if _, err := s.repo.BeginRotation(ctx, prev.Metadata.ID, patch, now); err != nil {
		if doTok && ctrl != nil {
			ctrl.ConfirmTokenWindow(prev.Metadata.ID)
		}
		if doAgent && agentCtrl != nil {
			agentCtrl.ConfirmAgentTokenWindow(prev.Metadata.ID)
		}
		return nil, err
	}

	_, callErr := conns.Call(ctx, prev.Metadata.ID, xrpc.MethodTokenRotate, rpc, callTimeout)
	if callErr == nil {
		if ctrl != nil {
			if doTok {
				ctrl.ConfirmTokenWindow(prev.Metadata.ID)
			}
			if len(roles) > 0 {
				ctrl.InvalidateServer(prev.Metadata.ID, false, roles...)
			}
		}
		if agentCtrl != nil && doAgent {
			agentCtrl.ConfirmAgentTokenWindow(prev.Metadata.ID)
		}
		if err := s.repo.FinishRotation(ctx, prev.Metadata.ID); err != nil {
			return nil, err
		}
		return s.repo.ByID(ctx, prev.Metadata.ID)
	}

	var xe *xrpc.Error
	if errors.Is(callErr, agentconn.ErrOffline) || errors.As(callErr, &xe) {
		if ctrl != nil && doTok {
			ctrl.ConfirmTokenWindow(prev.Metadata.ID)
		}
		if agentCtrl != nil && doAgent {
			agentCtrl.ConfirmAgentTokenWindow(prev.Metadata.ID)
		}
		if rerr := s.repo.RollbackRotation(ctx, prev); rerr != nil {
			s.logger.Error("回滚失败的凭据轮换写库出错", "server", prev.Spec.Name, "err", rerr)
		}
		return nil, nodeError(prev.Spec.Name, xrpc.MethodTokenRotate, callTimeout, callErr)
	}

	// 已发出推送但等待确认超时（或连接中途断开）：保留新凭据与 rotation_pending=true，保留内存 30 秒双凭据窗口自然收敛。
	return s.repo.ByID(ctx, prev.Metadata.ID)
}

func (s *Service) tokenRevoke(ctx context.Context, inv *command.Invocation) (any, error) {
	if err := requireAdmin(ctx); err != nil {
		return nil, err
	}
	doTok, doAgent, doPull, roles, err := parseTokenScope(inv.String("which", "all"))
	if err != nil {
		return nil, err
	}
	srv, err := s.byName(ctx, inv.Arg(0))
	if err != nil {
		return nil, err
	}
	fresh, err := core.NewTokens(3)
	if err != nil {
		return nil, v1.Wrap(v1.CodeInternal, "生成新凭据失败", err)
	}
	var patch core.TokensPatch
	if doTok {
		patch.Token = &fresh[0]
	}
	if doAgent {
		patch.AgentToken = &fresh[1]
	}
	if doPull {
		patch.PullToken = &fresh[2]
	}
	markPending := doTok && srv.Status.Status != core.StatusPending
	updated, err := s.repo.RevokeTokens(ctx, srv.Metadata.ID, patch, time.Now().UTC(), markPending)
	if err != nil {
		return nil, err
	}
	if tc, ok := s.getConns().(tokenController); ok {
		tc.InvalidateServer(srv.Metadata.ID, doTok, roles...)
	}
	return updated, nil
}
