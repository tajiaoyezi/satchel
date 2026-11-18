package securechan

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"sync"
	"time"
)

const (
	// HeaderSession 携带双向 HTTP securechan 的会话标识（不碰 Authorization 头）。
	HeaderSession = "X-Satchel-Session"
	// HeaderNonce 携带节点入站端口静默认证的随机防重放 nonce。
	HeaderNonce = "X-Satchel-Nonce"
	// HeaderAuth 携带基于 HMAC-SHA256 的静默认证签名（十六进制小写）。
	HeaderAuth = "X-Satchel-Auth"
	// UserAgentPrefix 是 Satchel 主控与节点内部 HTTP 请求约定的 User-Agent 前缀。
	UserAgentPrefix = "satchel-"

	// DefaultNonceTTL 是静默认证 nonce 的本地防重放窗口时长（无需跨机时钟同步）。
	DefaultNonceTTL = 5 * time.Minute
	defaultNonceCap = 4096
)

// RandomNonce 生成 16 字节随机十六进制 nonce。
func RandomNonce() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// SignSilentAuth 对节点入站 HTTP 请求计算 HMAC-SHA256(token, method+"\n"+path+"\n"+nonce)。
// token 为空时返回空串。
func SignSilentAuth(token, method, path, nonce string) string {
	if token == "" {
		return ""
	}
	mac := hmac.New(sha256.New, []byte(token))
	mac.Write([]byte(method))
	mac.Write([]byte("\n"))
	mac.Write([]byte(path))
	mac.Write([]byte("\n"))
	mac.Write([]byte(nonce))
	return hex.EncodeToString(mac.Sum(nil))
}

// VerifySilentAuth 使用常量时间比对校验静默认证签名；支持传入多把候选令牌（例如轮换双窗期），
// 返回命中的令牌（绝不允许空候选令牌放行）。
func VerifySilentAuth(tokens []string, method, path, nonce, sig string) (matched string, ok bool) {
	if method == "" || path == "" || nonce == "" || sig == "" {
		return "", false
	}
	got, err := hex.DecodeString(sig)
	if err != nil || len(got) != sha256.Size {
		return "", false
	}
	msg := method + "\n" + path + "\n" + nonce
	for _, tok := range tokens {
		if tok == "" {
			continue
		}
		mac := hmac.New(sha256.New, []byte(tok))
		mac.Write([]byte(msg))
		expected := mac.Sum(nil)
		if hmac.Equal(expected, got) && !ok {
			matched, ok = tok, true
		}
	}
	return matched, ok
}

type nonceEntry struct {
	expiresAt time.Time
}

// NonceWindow 在接收端维护短期防重放 nonce 集合，不依赖发送方系统时钟同步。
type NonceWindow struct {
	mu         sync.Mutex
	ttl        time.Duration
	maxEntries int
	now        func() time.Time
	seen       map[string]nonceEntry
}

// NewNonceWindow 构造防重放窗口；零值参数取默认值。
func NewNonceWindow(ttl time.Duration, maxEntries int, now func() time.Time) *NonceWindow {
	if ttl <= 0 {
		ttl = DefaultNonceTTL
	}
	if maxEntries <= 0 {
		maxEntries = defaultNonceCap
	}
	if now == nil {
		now = time.Now
	}
	return &NonceWindow{
		ttl:        ttl,
		maxEntries: maxEntries,
		now:        now,
		seen:       make(map[string]nonceEntry),
	}
}

// Allow 判断 nonce 是否有效且在窗口内首次出现；仅对非空、合法长度且未重放的 nonce 返回 true。
func (w *NonceWindow) Allow(nonce string) bool {
	if nonce == "" || len(nonce) > 128 {
		return false
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	now := w.now()
	for k, e := range w.seen {
		if !now.Before(e.expiresAt) {
			delete(w.seen, k)
		}
	}
	if _, exists := w.seen[nonce]; exists {
		return false
	}
	if len(w.seen) >= w.maxEntries {
		var (
			victimKey string
			victimExp time.Time
		)
		for k, e := range w.seen {
			if victimKey == "" || e.expiresAt.Before(victimExp) {
				victimKey, victimExp = k, e.expiresAt
			}
		}
		if victimKey != "" {
			delete(w.seen, victimKey)
		}
	}
	w.seen[nonce] = nonceEntry{expiresAt: now.Add(w.ttl)}
	return true
}
