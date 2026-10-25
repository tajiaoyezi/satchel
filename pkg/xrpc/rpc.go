package xrpc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"
)

// DefaultTimeout 是请求的默认超时。
const DefaultTimeout = 30 * time.Second

var (
	// ErrTimeout 是超时没等到回复。
	ErrTimeout = errors.New("xrpc：等回复超时")
	// ErrClosed 是连接已断开，等不到回复了。
	ErrClosed = errors.New("xrpc：连接已断开")
)

// Caller 是一条连接上发起请求的一方：生成请求编号、记下等待者，读循环收到回复时交给 Deliver，
// 连接断开时 CloseAll 放出所有等待者。
type Caller struct {
	mu      sync.Mutex
	next    uint64
	pending map[string]chan Reply
	closed  error
}

// NewCaller 建一个空的等待表。
func NewCaller() *Caller { return &Caller{pending: map[string]chan Reply{}} }

// Call 经 conn 发一个请求并等回复：timeout 为 0 时用 DefaultTimeout；对方回了错误时返回 *Error。
func (c *Caller) Call(ctx context.Context, conn *Conn, method string, params any, timeout time.Duration) (json.RawMessage, error) {
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	var raw json.RawMessage
	if params != nil {
		b, err := json.Marshal(params)
		if err != nil {
			return nil, err
		}
		raw = b
	}
	c.mu.Lock()
	if c.closed != nil {
		c.mu.Unlock()
		return nil, c.closed
	}
	c.next++
	id := strconv.FormatUint(c.next, 10)
	ch := make(chan Reply, 1)
	c.pending[id] = ch
	c.mu.Unlock()
	defer c.forget(id)

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if err := conn.Write(ctx, TypeCall, Call{ID: id, Method: method, Params: raw, TimeoutMS: timeout.Milliseconds()}); err != nil {
		return nil, fmt.Errorf("xrpc：发请求失败：%w", err)
	}
	select {
	case r, ok := <-ch:
		if !ok {
			return nil, c.err()
		}
		if r.Error != nil {
			return nil, r.Error
		}
		return r.Result, nil
	case <-ctx.Done():
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, ErrTimeout
		}
		return nil, ctx.Err()
	}
}

func (c *Caller) forget(id string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.pending, id)
}

func (c *Caller) err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed != nil {
		return c.closed
	}
	return ErrClosed
}

// Deliver 把一条回复交给等它的请求；没有人等（已超时或编号不认识）时返回 false。
func (c *Caller) Deliver(r Reply) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	ch, ok := c.pending[r.ID]
	if !ok {
		return false
	}
	delete(c.pending, r.ID)
	ch <- r
	return true
}

// CloseAll 在连接断开时放出所有等待者，之后的 Call 直接返回 err（为 nil 时用 ErrClosed）。
func (c *Caller) CloseAll(err error) {
	if err == nil {
		err = ErrClosed
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed != nil {
		return
	}
	c.closed = err
	for id, ch := range c.pending {
		close(ch)
		delete(c.pending, id)
	}
}

// Handler 是一个方法：params 是请求里的参数原文，返回值序列化成回复的结果。
type Handler func(ctx context.Context, params json.RawMessage) (any, error)

// Methods 是按方法名分派的方法表（节点一侧）。
type Methods map[string]Handler

// Serve 执行一个请求并给出回复：方法不认识回 CodeUnknownMethod；请求带了超时就在超时内做完。
func (m Methods) Serve(ctx context.Context, call Call) Reply {
	h, ok := m[call.Method]
	if !ok {
		return Reply{ID: call.ID, Error: &Error{Code: CodeUnknownMethod, Message: fmt.Sprintf("不认识的方法 %q", call.Method)}}
	}
	if call.TimeoutMS > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(call.TimeoutMS)*time.Millisecond)
		defer cancel()
	}
	result, err := h(ctx, call.Params)
	if err != nil {
		var e *Error
		if errors.As(err, &e) {
			return Reply{ID: call.ID, Error: e}
		}
		return Reply{ID: call.ID, Error: &Error{Code: CodeFailed, Message: err.Error()}}
	}
	raw, err := json.Marshal(result)
	if err != nil {
		return Reply{ID: call.ID, Error: &Error{Code: CodeFailed, Message: "结果序列化失败：" + err.Error()}}
	}
	return Reply{ID: call.ID, Result: raw}
}
