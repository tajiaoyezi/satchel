package xrpc

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// serve 起一个 websocket 服务端，每条连接交给 fn（在服务端的 goroutine 里跑）；返回 ws:// 地址。
func serve(t *testing.T, fn func(*Conn)) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		c := NewConn(ws)
		defer c.WS().CloseNow()
		fn(c)
	}))
	t.Cleanup(srv.Close)
	return "ws" + strings.TrimPrefix(srv.URL, "http")
}

func dial(t *testing.T, url string) *Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ws, _, err := websocket.Dial(ctx, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ws.CloseNow() })
	return NewConn(ws)
}

func ctx(t *testing.T) context.Context {
	t.Helper()
	c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	return c
}

// 握手之后双方互发加密消息；服务端把收到的心跳原样确认回来。
func TestHandshakeAndMessages(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	url := serve(t, func(c *Conn) {
		if err := ServerHandshake(context.Background(), c, priv); err != nil {
			return
		}
		m, err := c.Read(context.Background())
		if err != nil || m.Type != TypeHeartbeat {
			return
		}
		var hb Heartbeat
		_ = Decode(m, &hb)
		_ = c.Write(context.Background(), TypeHeartbeatAck, HeartbeatAck{ServerTime: hb.BootTime})
		_, _ = c.Read(context.Background()) // 等对方关
	})
	c := dial(t, url)
	if err := c.Write(ctx(t), TypeHeartbeat, nil); !errors.Is(err, ErrNotSecured) {
		t.Fatalf("握手之前不能发加密消息：%v", err)
	}
	if err := ClientHandshake(ctx(t), c, pub); err != nil {
		t.Fatal(err)
	}
	if err := c.WritePlain(ctx(t), TypeAuth, Auth{}); !errors.Is(err, ErrNotSecured) {
		t.Fatalf("握手之后不能再发明文：%v", err)
	}
	boot := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	if err := c.Write(ctx(t), TypeHeartbeat, Heartbeat{BootTime: boot}); err != nil {
		t.Fatal(err)
	}
	m, err := c.Read(ctx(t))
	if err != nil || m.Type != TypeHeartbeatAck {
		t.Fatalf("应当收到心跳确认：%+v %v", m, err)
	}
	var ack HeartbeatAck
	if err := Decode(m, &ack); err != nil || !ack.ServerTime.Equal(boot) {
		t.Fatalf("确认的内容不对：%+v %v", ack, err)
	}
}

// 主控公钥不对：节点验签失败，返回 ErrBadSignature，还没有发出任何加密消息。
func TestClientRejectsWrongMasterKey(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	otherPub, _, _ := ed25519.GenerateKey(rand.Reader)
	got := make(chan error, 1)
	url := serve(t, func(c *Conn) {
		if err := ServerHandshake(context.Background(), c, priv); err != nil {
			got <- err
			return
		}
		_, err := c.Read(context.Background())
		got <- err
	})
	c := dial(t, url)
	if err := ClientHandshake(ctx(t), c, otherPub); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("应当验签失败：%v", err)
	}
	c.WS().CloseNow()
	select {
	case err := <-got:
		if err == nil {
			t.Fatal("服务端不应当收到任何消息")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("服务端没有结束")
	}
	if err := ClientHandshake(ctx(t), dial(t, url), nil); err == nil {
		t.Fatal("没有主控公钥时应当直接报错，不跳过握手")
	}
}

// 握手之后对方发明文帧：读的一方返回 ErrPlaintext；握手阶段对方发二进制帧：返回 ErrNotPlain。
func TestFrameRules(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	readErr := make(chan error, 2)
	url := serve(t, func(c *Conn) {
		if err := ServerHandshake(context.Background(), c, priv); err != nil {
			readErr <- err
			return
		}
		_, err := c.Read(context.Background())
		readErr <- err
	})
	c := dial(t, url)
	if err := ClientHandshake(ctx(t), c, pub); err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(Message{Type: TypeAuth})
	if err := c.WS().Write(ctx(t), websocket.MessageText, data); err != nil {
		t.Fatal(err)
	}
	if err := <-readErr; !errors.Is(err, ErrPlaintext) {
		t.Fatalf("握手之后的明文帧应当报 ErrPlaintext：%v", err)
	}

	c2 := dial(t, url)
	if err := c2.WS().Write(ctx(t), websocket.MessageBinary, []byte{1, 2, 3}); err != nil {
		t.Fatal(err)
	}
	if err := <-readErr; !errors.Is(err, ErrNotPlain) {
		t.Fatalf("握手阶段的二进制帧应当报 ErrNotPlain：%v", err)
	}
}

// 请求与回复：服务端发请求，客户端用方法表作答；不认识的方法回 unknown_method；超时；断开时等待者拿到 ErrClosed。
func TestCallReply(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	type result struct {
		info    AgentInfo
		unknown error
		timeout error
		closed  error
	}
	done := make(chan result, 1)
	url := serve(t, func(c *Conn) {
		if err := ServerHandshake(context.Background(), c, priv); err != nil {
			return
		}
		caller := NewCaller()
		go func() {
			for {
				m, err := c.Read(context.Background())
				if err != nil {
					caller.CloseAll(nil)
					return
				}
				if m.Type == TypeReply {
					var r Reply
					_ = Decode(m, &r)
					caller.Deliver(r)
				}
			}
		}()
		var res result
		bg := context.Background()
		raw, err := caller.Call(bg, c, MethodAgentInfo, nil, 0)
		if err == nil {
			_ = json.Unmarshal(raw, &res.info)
		}
		_, res.unknown = caller.Call(bg, c, "no.such", nil, 0)
		_, res.timeout = caller.Call(bg, c, "slow", nil, 200*time.Millisecond)
		go func() {
			time.Sleep(100 * time.Millisecond)
			_ = c.Write(bg, TypeHeartbeatAck, nil) // 让客户端知道该断开了
		}()
		_, res.closed = caller.Call(bg, c, "slow", nil, 5*time.Second)
		done <- res
	})
	c := dial(t, url)
	if err := ClientHandshake(ctx(t), c, pub); err != nil {
		t.Fatal(err)
	}
	methods := Methods{
		MethodAgentInfo: func(context.Context, json.RawMessage) (any, error) {
			return AgentInfo{AgentVersion: "1.2.3", ControlProtocol: ControlProtocol}, nil
		},
		"slow": func(ctx context.Context, _ json.RawMessage) (any, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		},
	}
	go func() {
		for {
			m, err := c.Read(context.Background())
			if err != nil {
				return
			}
			switch m.Type {
			case TypeCall:
				var call Call
				_ = Decode(m, &call)
				go func() { _ = c.Write(context.Background(), TypeReply, methods.Serve(context.Background(), call)) }()
			case TypeHeartbeatAck:
				c.WS().CloseNow()
				return
			}
		}
	}()
	var res result
	select {
	case res = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("服务端没有结束")
	}
	if res.info.AgentVersion != "1.2.3" || res.info.ControlProtocol != ControlProtocol {
		t.Fatalf("agent.info 的结果不对：%+v", res.info)
	}
	var e *Error
	if !errors.As(res.unknown, &e) || e.Code != CodeUnknownMethod {
		t.Fatalf("不认识的方法应当回 unknown_method：%v", res.unknown)
	}
	if !errors.Is(res.timeout, ErrTimeout) {
		t.Fatalf("应当超时：%v", res.timeout)
	}
	if !errors.Is(res.closed, ErrClosed) {
		t.Fatalf("断开时应当拿到 ErrClosed：%v", res.closed)
	}
}
