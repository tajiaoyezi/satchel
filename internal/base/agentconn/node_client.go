package agentconn

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/satchel/satchel/pkg/securechan"
	"github.com/satchel/satchel/pkg/xrpc"
)

const (
	roleManage = "manage"
	rolePull   = "pull"
)

type nodeSessionKey struct {
	serverID int64
	role     string
	baseURL  string
	token    string
}

type nodeSession struct {
	sessionID string
	sess      *securechan.Session
	expiresAt time.Time
}

type nodeEndpoint struct {
	baseURL string
	host    string
	v6      bool
}

type errCandidateUnreachable struct{ err error }

func (e errCandidateUnreachable) Error() string { return e.err.Error() }
func (e errCandidateUnreachable) Unwrap() error { return e.err }

// nodeClient 负责 Master → Agent 加密 HTTP 管理端与只读轮询端通信（SilentAuth + 双向签名 1.5 RTT 握手 + 412 重试）。
type nodeClient struct {
	m         *Manager
	transport *http.Transport
	mu        sync.Mutex
	sessions  map[nodeSessionKey]nodeSession
}

func newNodeClient(m *Manager) *nodeClient {
	tr := &http.Transport{
		DialContext: (&net.Dialer{
			Timeout: m.cfg.HTTPDialTimeout,
		}).DialContext,
		MaxIdleConns:        64,
		MaxIdleConnsPerHost: 4,
		IdleConnTimeout:     90 * time.Second,
	}
	return &nodeClient{
		m:         m,
		transport: tr,
		sessions:  make(map[nodeSessionKey]nodeSession),
	}
}

func (c *nodeClient) CloseIdleConnections() {
	c.transport.CloseIdleConnections()
}

func (c *nodeClient) ClearServerSessions(serverID int64, roles ...string) {
	if serverID <= 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for k := range c.sessions {
		if k.serverID != serverID {
			continue
		}
		if len(roles) == 0 {
			delete(c.sessions, k)
			continue
		}
		for _, r := range roles {
			if k.role == r {
				delete(c.sessions, k)
				break
			}
		}
	}
}

func (c *nodeClient) getSession(key nodeSessionKey) (nodeSession, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	ns, ok := c.sessions[key]
	if !ok {
		return nodeSession{}, false
	}
	if !c.m.cfg.Now().Before(ns.expiresAt) {
		delete(c.sessions, key)
		return nodeSession{}, false
	}
	return ns, true
}

func (c *nodeClient) putSession(key nodeSessionKey, ns nodeSession) {
	c.mu.Lock()
	c.sessions[key] = ns
	c.mu.Unlock()
}

func (c *nodeClient) deleteSession(key nodeSessionKey) {
	c.mu.Lock()
	delete(c.sessions, key)
	c.mu.Unlock()
}

// CallManage 在无活跃 WS 时按模式与寻址顺序向节点管理口（listen_port，默认 23889）下发加密 HTTP RPC。
func (c *nodeClient) CallManage(ctx context.Context, serverID int64, method string, params any, timeout time.Duration) (json.RawMessage, error) {
	t, ok, err := c.m.targetInfo(ctx, serverID)
	if err != nil {
		return nil, err
	}
	if !ok || t.ConnectionMode == "websocket" || t.AgentToken == "" {
		return nil, ErrOffline
	}
	port := t.ListenPort
	if port <= 0 {
		port = 23889
	}
	candidates := buildEndpoints([]string{t.Domain, t.IPAddress, t.DomainV6, t.IPAddressV6}, port)
	if len(candidates) == 0 {
		return nil, ErrOffline
	}
	tokens := c.m.agentCandidateTokens(serverID, t.AgentToken, method)
	var lastErr error
	for _, tok := range tokens {
		raw, _, err := c.callEndpoints(ctx, serverID, roleManage, tok, candidates, method, params, timeout)
		if err == nil {
			return raw, nil
		}
		if errors.Is(err, ErrOffline) {
			lastErr = err
			continue
		}
		return nil, err
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, ErrOffline
}

// PullTraffic 按轮询地址顺序从节点只读轮询口（pull_port，默认 23890）拉取 traffic.snapshot。
func (c *nodeClient) PullTraffic(ctx context.Context, serverID int64, timeout time.Duration) (xrpc.TrafficSnapshot, string, bool, error) {
	t, ok, err := c.m.targetInfo(ctx, serverID)
	if err != nil {
		return xrpc.TrafficSnapshot{}, "", false, err
	}
	if !ok || t.PullToken == "" {
		return xrpc.TrafficSnapshot{}, "", false, ErrOffline
	}
	port := t.PullPort
	if port <= 0 {
		port = 23890
	}
	candidates := buildEndpoints([]string{t.PullAddress, t.IPAddress, t.Domain, t.PullAddressV6, t.IPAddressV6, t.DomainV6}, port)
	if len(candidates) == 0 {
		return xrpc.TrafficSnapshot{}, "", false, ErrOffline
	}
	raw, ep, err := c.callEndpointsPath(ctx, serverID, rolePull, t.PullToken, candidates, xrpc.HTTPNodePathRPC, xrpc.MethodTrafficSnapshot, nil, timeout)
	if err != nil {
		return xrpc.TrafficSnapshot{}, "", false, err
	}
	var snap xrpc.TrafficSnapshot
	if err := json.Unmarshal(raw, &snap); err != nil {
		return xrpc.TrafficSnapshot{}, "", false, fmt.Errorf("轮询口快照解不开：%w", err)
	}
	return snap, ep.host, ep.v6, nil
}

// PullSpeed 按轮询地址顺序从节点只读轮询口（pull_port，默认 23890）拉取 /api/child/speed（speed.get）。
func (c *nodeClient) PullSpeed(ctx context.Context, serverID int64, timeout time.Duration) (xrpc.SpeedReport, string, bool, error) {
	t, ok, err := c.m.targetInfo(ctx, serverID)
	if err != nil {
		return xrpc.SpeedReport{}, "", false, err
	}
	if !ok || t.PullToken == "" {
		return xrpc.SpeedReport{}, "", false, ErrOffline
	}
	port := t.PullPort
	if port <= 0 {
		port = 23890
	}
	candidates := buildEndpoints([]string{t.PullAddress, t.IPAddress, t.Domain, t.PullAddressV6, t.IPAddressV6, t.DomainV6}, port)
	if len(candidates) == 0 {
		return xrpc.SpeedReport{}, "", false, ErrOffline
	}
	raw, ep, err := c.callEndpointsPath(ctx, serverID, rolePull, t.PullToken, candidates, xrpc.HTTPNodePathPullSpeed, xrpc.MethodSpeedGet, nil, timeout)
	if err != nil {
		return xrpc.SpeedReport{}, "", false, err
	}
	var rep xrpc.SpeedReport
	if err := json.Unmarshal(raw, &rep); err != nil {
		return xrpc.SpeedReport{}, "", false, fmt.Errorf("轮询口网速解不开：%w", err)
	}
	return rep, ep.host, ep.v6, nil
}

func buildEndpoints(hosts []string, port int64) []nodeEndpoint {
	out := make([]nodeEndpoint, 0, len(hosts))
	seen := make(map[string]struct{}, len(hosts))
	portStr := strconv.FormatInt(port, 10)
	for _, raw := range hosts {
		h := strings.TrimSpace(raw)
		if h == "" {
			continue
		}
		if _, dup := seen[h]; dup {
			continue
		}
		seen[h] = struct{}{}
		hostOnly := h
		addr := net.JoinHostPort(h, portStr)
		if parsedHost, _, err := net.SplitHostPort(h); err == nil {
			addr = h
			hostOnly = parsedHost
		}
		out = append(out, nodeEndpoint{
			baseURL: "http://" + addr,
			host:    hostOnly,
			v6:      isV6(hostOnly),
		})
	}
	return out
}

func (c *nodeClient) callEndpoints(ctx context.Context, serverID int64, role, token string, candidates []nodeEndpoint, method string, params any, timeout time.Duration) (json.RawMessage, nodeEndpoint, error) {
	return c.callEndpointsPath(ctx, serverID, role, token, candidates, xrpc.HTTPNodePathRPC, method, params, timeout)
}

func (c *nodeClient) callEndpointsPath(ctx context.Context, serverID int64, role, token string, candidates []nodeEndpoint, rpcPath, method string, params any, timeout time.Duration) (json.RawMessage, nodeEndpoint, error) {
	if timeout <= 0 {
		timeout = xrpc.DefaultTimeout
	}
	var lastErr error
	for _, ep := range candidates {
		raw, err := c.callOne(ctx, serverID, role, token, ep.baseURL, rpcPath, method, params, timeout)
		if err == nil {
			return raw, ep, nil
		}
		var unreach errCandidateUnreachable
		if errors.As(err, &unreach) {
			lastErr = unreach.err
			continue
		}
		return nil, nodeEndpoint{}, err
	}
	if lastErr != nil {
		return nil, nodeEndpoint{}, fmt.Errorf("%w：%v", ErrOffline, lastErr)
	}
	return nil, nodeEndpoint{}, ErrOffline
}

func (c *nodeClient) callOne(ctx context.Context, serverID int64, role, token, baseURL, rpcPath, method string, params any, timeout time.Duration) (json.RawMessage, error) {
	rctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var rawParams json.RawMessage
	if params != nil {
		b, err := json.Marshal(params)
		if err != nil {
			return nil, fmt.Errorf("编码请求参数失败：%w", err)
		}
		rawParams = b
	}
	reqID, err := securechan.GenerateSessionID()
	if err != nil {
		return nil, err
	}
	callBytes, err := json.Marshal(xrpc.Call{ID: reqID, Method: method, Params: rawParams})
	if err != nil {
		return nil, err
	}
	key := nodeSessionKey{serverID: serverID, role: role, baseURL: baseURL, token: token}

	for attempt := 0; attempt < 2; attempt++ {
		ns, ok := c.getSession(key)
		if !ok {
			ns, err = c.handshake(rctx, key)
			if err != nil {
				return nil, errCandidateUnreachable{err: err}
			}
			c.putSession(key, ns)
		}
		env, err := ns.sess.Encrypt(callBytes)
		if err != nil {
			return nil, err
		}
		respBytes, status, gotConn, remoteIP, err := c.doSignedHTTP(rctx, http.MethodPost, baseURL+rpcPath, rpcPath, token, ns.sessionID, env)
		if err != nil {
			if !gotConn {
				c.deleteSession(key)
				return nil, errCandidateUnreachable{err: err}
			}
			if errors.Is(err, context.DeadlineExceeded) || errors.Is(rctx.Err(), context.DeadlineExceeded) {
				return nil, xrpc.ErrTimeout
			}
			return nil, err
		}
		if remoteIP != "" {
			c.m.RecordPeerIP(serverID, remoteIP)
		}
		if status == http.StatusPreconditionFailed {
			c.deleteSession(key)
			continue
		}
		if status != http.StatusOK {
			return nil, fmt.Errorf("节点返回 HTTP %d：%s", status, strings.TrimSpace(string(respBytes)))
		}
		plain, err := ns.sess.Decrypt(respBytes)
		if err != nil {
			return nil, fmt.Errorf("解包节点响应失败：%w", err)
		}
		var reply xrpc.Reply
		if err := json.Unmarshal(plain, &reply); err != nil {
			return nil, fmt.Errorf("解析节点响应失败：%w", err)
		}
		if reply.Error != nil {
			return nil, reply.Error
		}
		return reply.Result, nil
	}
	return nil, errors.New("节点持续返回 412，加密会话重协商失败")
}

func (c *nodeClient) handshake(ctx context.Context, key nodeSessionKey) (nodeSession, error) {
	// 1. /handshake/init 取节点临时公钥与 handshake_id
	initBytes, status, _, _, err := c.doSignedHTTP(ctx, http.MethodPost, key.baseURL+xrpc.HTTPNodePathHandshakeInit, xrpc.HTTPNodePathHandshakeInit, key.token, "", []byte("{}"))
	if err != nil {
		return nodeSession{}, err
	}
	if status != http.StatusOK {
		return nodeSession{}, fmt.Errorf("节点 /handshake/init 返回 %d", status)
	}
	var initResp xrpc.NodeHandshakeInitResp
	if err := json.Unmarshal(initBytes, &initResp); err != nil || initResp.HandshakeID == "" || len(initResp.AgentEphemeral) != 32 {
		return nodeSession{}, errors.New("节点 /handshake/init 响应不合法")
	}
	agentEph, err := ecdh.X25519().NewPublicKey(initResp.AgentEphemeral)
	if err != nil {
		return nodeSession{}, fmt.Errorf("节点临时公钥不合法：%w", err)
	}
	masterEphPriv, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return nodeSession{}, err
	}
	shared, err := masterEphPriv.ECDH(agentEph)
	if err != nil {
		return nodeSession{}, err
	}
	masterEphBytes := masterEphPriv.PublicKey().Bytes()
	sess, err := securechan.DeriveSession(shared, initResp.AgentEphemeral, masterEphBytes, true)
	if err != nil {
		return nodeSession{}, err
	}
	sig := securechan.SignHandshake(c.m.cfg.Identity, initResp.AgentEphemeral, masterEphBytes)
	finReq, err := json.Marshal(xrpc.NodeHandshakeFinishReq{
		HandshakeID:     initResp.HandshakeID,
		MasterEphemeral: masterEphBytes,
		Signature:       sig,
	})
	if err != nil {
		return nodeSession{}, err
	}

	// 2. /handshake/finish 完成双向签名校验并取回 session_id
	finBytes, status, _, _, err := c.doSignedHTTP(ctx, http.MethodPost, key.baseURL+xrpc.HTTPNodePathHandshakeFinish, xrpc.HTTPNodePathHandshakeFinish, key.token, "", finReq)
	if err != nil {
		return nodeSession{}, err
	}
	if status != http.StatusOK {
		return nodeSession{}, fmt.Errorf("节点 /handshake/finish 返回 %d", status)
	}
	var finResp xrpc.NodeHandshakeFinishResp
	if err := json.Unmarshal(finBytes, &finResp); err != nil || finResp.SessionID == "" {
		return nodeSession{}, errors.New("节点 /handshake/finish 响应不合法")
	}
	return nodeSession{
		sessionID: finResp.SessionID,
		sess:      sess,
		expiresAt: c.m.cfg.Now().Add(c.m.cfg.SessionTTL),
	}, nil
}

func (c *nodeClient) doSignedHTTP(ctx context.Context, method, fullURL, path, token, sessionID string, body []byte) (respBody []byte, status int, gotConn bool, remoteIP string, err error) {
	nonce, err := securechan.RandomNonce()
	if err != nil {
		return nil, 0, false, "", err
	}
	tctx := httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{
		GotConn: func(info httptrace.GotConnInfo) {
			gotConn = true
			if info.Conn != nil && info.Conn.RemoteAddr() != nil {
				if host, _, serr := net.SplitHostPort(info.Conn.RemoteAddr().String()); serr == nil {
					remoteIP = host
				}
			}
		},
	})
	var bodyReader io.Reader
	if len(body) > 0 {
		bodyReader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(tctx, method, fullURL, bodyReader)
	if err != nil {
		return nil, 0, false, "", err
	}
	ver := c.m.cfg.Version
	if ver == "" {
		ver = DevVersion
	}
	req.Header.Set("User-Agent", securechan.UserAgentPrefix+"master/"+ver)
	req.Header.Set(securechan.HeaderNonce, nonce)
	req.Header.Set(securechan.HeaderAuth, securechan.SignSilentAuth(token, method, path, nonce))
	if sessionID != "" {
		req.Header.Set(securechan.HeaderSession, sessionID)
	}
	client := &http.Client{Transport: c.transport}
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, gotConn, remoteIP, err
	}
	defer resp.Body.Close()
	data, rerr := io.ReadAll(io.LimitReader(resp.Body, xrpc.ReadLimit))
	if rerr != nil {
		return nil, resp.StatusCode, gotConn, remoteIP, rerr
	}
	return data, resp.StatusCode, gotConn, remoteIP, nil
}
