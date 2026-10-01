package db

import (
	"context"
	"sync"
	"sync/atomic"
	"time"
)

// WriteGate 是「写入暂停」的开关（master-db-migration「拷贝期间阻塞写入」、master-self-update「应用升级的步骤」）：
// 在线迁移拿着 SQLite 的写锁时、自升级从备份开始到 exec 之前打开，命令执行链、会话入口、审计、令牌的最后使用时间与安全事件
// 都看它，提前拒绝或跳过写库。打开时带上原因与下一步（拦下的请求按它回应）。零值是关着的；多个 goroutine 并发读写安全。
//
// 进门的地方用 Enter / Leave 登记正在处理的请求：打开开关之后，Drain 等开关打开之前已经进门的请求都走完——
// 它们可能在开关打开之后才提交写入（自升级要在它们走完之后才做升级前备份）。
type WriteGate struct {
	suspended atomic.Bool
	mu        sync.Mutex
	reason    string
	next      string
	inflight  int
}

// Suspend 打开开关：从此写入被挡；reason 与 next 是被拦下的请求看到的原因与下一步。
func (g *WriteGate) Suspend(reason, next string) {
	g.mu.Lock()
	g.reason, g.next = reason, next
	g.suspended.Store(true)
	g.mu.Unlock()
}

// Resume 关掉开关。
func (g *WriteGate) Resume() { g.suspended.Store(false) }

// Suspended 报告写入是否被挡；nil 当作关着。
func (g *WriteGate) Suspended() bool { return g != nil && g.suspended.Load() }

// Why 返回最近一次打开时给的原因与下一步。
func (g *WriteGate) Why() (reason, next string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.reason, g.next
}

// Enter 登记一个进门的请求；开关开着时不登记、返回 false（调用方拒绝它）。返回 true 时调用方处理完要调 Leave。nil 当作关着。
func (g *WriteGate) Enter() bool {
	if g == nil {
		return true
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.suspended.Load() {
		return false
	}
	g.inflight++
	return true
}

// Leave 结束一个 Enter 登记的请求。
func (g *WriteGate) Leave() {
	if g == nil {
		return
	}
	g.mu.Lock()
	g.inflight--
	g.mu.Unlock()
}

type enteredKey struct{}

// WithEntered 标出这个请求已经经 Enter 登记进门：开关之后才打开也照常写库（它的写入在 Drain 之前完成，都在之后的备份里）。
func WithEntered(ctx context.Context) context.Context {
	return context.WithValue(ctx, enteredKey{}, true)
}

// Entered 报告请求是否经 Enter 登记进了门。审计据此决定：开关开着时，没登记的（放行的 job get / list）只记日志，登记过的照常写库。
func Entered(ctx context.Context) bool { v, _ := ctx.Value(enteredKey{}).(bool); return v }

// Drain 等 Enter 登记过、还没 Leave 的请求都走完；ctx 先到期就返回它的错误。
func (g *WriteGate) Drain(ctx context.Context) error {
	for {
		g.mu.Lock()
		n := g.inflight
		g.mu.Unlock()
		if n == 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
}
