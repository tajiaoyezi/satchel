package securechan

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"sync"
	"time"
)

const (
	// DefaultSessionTTL 是 HTTP securechan 会话的默认有效期。
	DefaultSessionTTL = time.Hour
	// DefaultCacheCapacity 是 SessionCache 的默认最大容量，防止恶意或洪水握手耗尽内存。
	DefaultCacheCapacity = 4096
)

var (
	// ErrSessionNotFound 表示会话不存在或已过期。
	ErrSessionNotFound = errors.New("securechan：会话不存在或已过期")
	// ErrSessionServerMismatch 表示会话已绑定到另一台服务器，不能跨服务器复用。
	ErrSessionServerMismatch = errors.New("securechan：会话已绑定其他服务器")
)

// GenerateSessionID 生成 32 字节密码学随机、base64url 无填充的会话标识（43 个字符）。
func GenerateSessionID() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// SessionCacheConfig 配置并发安全的 HTTP securechan 会话缓存。
type SessionCacheConfig struct {
	TTL        time.Duration
	MaxEntries int
	Now        func() time.Time
}

type cacheEntry struct {
	session   *Session
	serverID  int64
	role      string
	createdAt time.Time
	lastUsed  time.Time
	expiresAt time.Time
}

// SessionCache 维护 HTTP securechan 会话，带 TTL 过期清理、容量上限驱逐以及服务器绑定语义。
type SessionCache struct {
	mu         sync.Mutex
	ttl        time.Duration
	maxEntries int
	now        func() time.Time
	entries    map[string]*cacheEntry
}

// NewSessionCache 构造会话缓存。
func NewSessionCache(cfg SessionCacheConfig) *SessionCache {
	if cfg.TTL <= 0 {
		cfg.TTL = DefaultSessionTTL
	}
	if cfg.MaxEntries <= 0 {
		cfg.MaxEntries = DefaultCacheCapacity
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &SessionCache{
		ttl:        cfg.TTL,
		maxEntries: cfg.MaxEntries,
		now:        cfg.Now,
		entries:    make(map[string]*cacheEntry),
	}
}

func (c *SessionCache) pruneLocked(now time.Time) {
	for id, e := range c.entries {
		if !now.Before(e.expiresAt) {
			delete(c.entries, id)
		}
	}
}

func (c *SessionCache) evictOldestLocked() {
	var (
		victimID string
		victim   *cacheEntry
	)
	for id, e := range c.entries {
		if victim == nil ||
			e.lastUsed.Before(victim.lastUsed) ||
			(e.lastUsed.Equal(victim.lastUsed) && (e.createdAt.Before(victim.createdAt) || (e.createdAt.Equal(victim.createdAt) && id < victimID))) {
			victimID, victim = id, e
		}
	}
	if victimID != "" {
		delete(c.entries, victimID)
	}
}

// Put 写入或覆盖一条会话记录；超限时先清过期项，仍满则驱逐最近最少使用的项。
func (c *SessionCache) Put(id string, s *Session, serverID int64, role string) error {
	if id == "" || s == nil {
		return errors.New("securechan：会话 id 与实例都不能为空")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	c.pruneLocked(now)
	if _, exists := c.entries[id]; !exists && len(c.entries) >= c.maxEntries {
		c.evictOldestLocked()
	}
	c.entries[id] = &cacheEntry{
		session:   s,
		serverID:  serverID,
		role:      role,
		createdAt: now,
		lastUsed:  now,
		expiresAt: now.Add(c.ttl),
	}
	return nil
}

// Get 查找未过期的会话并刷新最近使用时间。
func (c *SessionCache) Get(id string) (s *Session, serverID int64, role string, ok bool) {
	if id == "" {
		return nil, 0, "", false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	e, exists := c.entries[id]
	if !exists {
		return nil, 0, "", false
	}
	now := c.now()
	if !now.Before(e.expiresAt) {
		delete(c.entries, id)
		return nil, 0, "", false
	}
	e.lastUsed = now
	return e.session, e.serverID, e.role, true
}

// BindServer 在会话首次认证通过后把会话绑定到唯一 serverID；若已绑定到不同服务器则拒绝。
func (c *SessionCache) BindServer(id string, serverID int64) error {
	if serverID <= 0 {
		return errors.New("securechan：serverID 必须大于 0")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	e, exists := c.entries[id]
	if !exists || !c.now().Before(e.expiresAt) {
		delete(c.entries, id)
		return ErrSessionNotFound
	}
	if e.serverID != 0 && e.serverID != serverID {
		return ErrSessionServerMismatch
	}
	e.serverID = serverID
	e.lastUsed = c.now()
	return nil
}

// Delete 删除单条会话。
func (c *SessionCache) Delete(id string) {
	c.mu.Lock()
	delete(c.entries, id)
	c.mu.Unlock()
}

// DeleteByServer 删除绑定到指定服务器的所有会话（吊销主控 token 时调用）。
func (c *SessionCache) DeleteByServer(serverID int64) {
	if serverID <= 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for id, e := range c.entries {
		if e.serverID == serverID {
			delete(c.entries, id)
		}
	}
}

// DeleteByServerRole 删除指定服务器与端口角色的会话（局部轮换/吊销时调用）。
func (c *SessionCache) DeleteByServerRole(serverID int64, role string) {
	if serverID <= 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for id, e := range c.entries {
		if e.serverID == serverID && e.role == role {
			delete(c.entries, id)
		}
	}
}

// Len 返回当前未过期的会话数量。
func (c *SessionCache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.pruneLocked(c.now())
	return len(c.entries)
}
