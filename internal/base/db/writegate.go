package db

import "sync/atomic"

// WriteGate 是「写入暂停」的开关（master-db-migration「拷贝期间阻塞写入」）：在线迁移拿着 SQLite 的写锁时打开，
// 命令执行链、会话入口、审计与令牌的最后使用时间都看它，提前拒绝或跳过写库，免得各自等满 busy_timeout 再失败。
// 零值是关着的；多个 goroutine 并发读写安全。
type WriteGate struct{ suspended atomic.Bool }

// Suspend 打开开关：从此写入被挡。
func (g *WriteGate) Suspend() { g.suspended.Store(true) }

// Resume 关掉开关。
func (g *WriteGate) Resume() { g.suspended.Store(false) }

// Suspended 报告写入是否被挡；nil 当作关着。
func (g *WriteGate) Suspended() bool { return g != nil && g.suspended.Load() }
