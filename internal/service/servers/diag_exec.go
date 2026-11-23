package servers

import (
	"context"
	"time"

	"github.com/satchel/satchel/internal/base/agentconn"
	v1 "github.com/satchel/satchel/pkg/api/v1"
	"github.com/satchel/satchel/pkg/xrpc"
)

const (
	diagRPCTimeout = 60 * time.Second
	execRPCTimeout = 15 * time.Second
)

type diagRuntimeConns interface {
	Connected(serverID int64) bool
	Runtime(serverID int64) (agentconn.NodeRuntime, bool)
	MasterMachineID() string
}

// ConnectedWS 报告指定服务器当前是否有活跃 WebSocket 连接。
func (s *Service) ConnectedWS(serverID int64) bool {
	c, ok := s.getConns().(diagRuntimeConns)
	if !ok || c == nil {
		return false
	}
	return c.Connected(serverID)
}

// NodeRuntime 返回指定服务器最近缓存的节点运行态元数据（含 PeerIP / MachineID / ExecEnabled）。
func (s *Service) NodeRuntime(serverID int64) (agentconn.NodeRuntime, bool) {
	c, ok := s.getConns().(diagRuntimeConns)
	if !ok || c == nil {
		return agentconn.NodeRuntime{}, false
	}
	return c.Runtime(serverID)
}

// MasterMachineID 返回主控运行态 machine_id（供 server exec 同机 fail-closed 判定使用）。
func (s *Service) MasterMachineID() string {
	c, ok := s.getConns().(diagRuntimeConns)
	if !ok || c == nil {
		return ""
	}
	return c.MasterMachineID()
}

// CallDiagRun 调用节点 diag.run 管理口 RPC。
func (s *Service) CallDiagRun(ctx context.Context, serverID int64, params xrpc.DiagRunParams) (*xrpc.DiagRunResult, error) {
	srv, err := s.repo.ByID(ctx, serverID)
	if err != nil {
		return nil, v1.Newf(v1.CodeNotFound, "没有叫 id=%d 的服务器", serverID)
	}
	var res xrpc.DiagRunResult
	if err := s.call(ctx, srv, xrpc.MethodDiagRun, params, &res, diagRPCTimeout); err != nil {
		return nil, err
	}
	if res.Items == nil {
		res.Items = []xrpc.DiagItemResult{}
	}
	return &res, nil
}

// CallExecStart 调用节点 exec.start 管理口 RPC。
func (s *Service) CallExecStart(ctx context.Context, serverID int64, params xrpc.ExecStartParams) (*xrpc.ExecStartResult, error) {
	srv, err := s.repo.ByID(ctx, serverID)
	if err != nil {
		return nil, v1.Newf(v1.CodeNotFound, "没有叫 id=%d 的服务器", serverID)
	}
	var res xrpc.ExecStartResult
	if err := s.call(ctx, srv, xrpc.MethodExecStart, params, &res, execRPCTimeout); err != nil {
		return nil, err
	}
	return &res, nil
}

// CallExecStatus 调用节点 exec.status 管理口 RPC。
func (s *Service) CallExecStatus(ctx context.Context, serverID int64, params xrpc.ExecStatusParams) (*xrpc.ExecStatusResult, error) {
	srv, err := s.repo.ByID(ctx, serverID)
	if err != nil {
		return nil, v1.Newf(v1.CodeNotFound, "没有叫 id=%d 的服务器", serverID)
	}
	var res xrpc.ExecStatusResult
	if err := s.call(ctx, srv, xrpc.MethodExecStatus, params, &res, callTimeout); err != nil {
		return nil, err
	}
	return &res, nil
}

// CallExecCancel 调用节点 exec.cancel 管理口 RPC。
func (s *Service) CallExecCancel(ctx context.Context, serverID int64, params xrpc.ExecCancelParams) (*xrpc.ExecCancelResult, error) {
	srv, err := s.repo.ByID(ctx, serverID)
	if err != nil {
		return nil, v1.Newf(v1.CodeNotFound, "没有叫 id=%d 的服务器", serverID)
	}
	var res xrpc.ExecCancelResult
	if err := s.call(ctx, srv, xrpc.MethodExecCancel, params, &res, callTimeout); err != nil {
		return nil, err
	}
	return &res, nil
}

// CallAgentInfo 调用节点 agent.info 管理口 RPC 并同步刷新内存运行态缓存。
func (s *Service) CallAgentInfo(ctx context.Context, serverID int64) (*xrpc.AgentInfo, error) {
	srv, err := s.repo.ByID(ctx, serverID)
	if err != nil {
		return nil, v1.Newf(v1.CodeNotFound, "没有叫 id=%d 的服务器", serverID)
	}
	var res xrpc.AgentInfo
	if err := s.call(ctx, srv, xrpc.MethodAgentInfo, nil, &res, callTimeout); err != nil {
		return nil, err
	}
	s.recordRuntime(srv.Metadata.ID, res)
	return &res, nil
}
