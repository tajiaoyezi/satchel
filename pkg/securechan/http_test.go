package securechan

import (
	"testing"
	"time"
)

func TestSessionCacheLifecycleAndEviction(t *testing.T) {
	now := time.Date(2026, 11, 18, 0, 0, 0, 0, time.UTC)
	cache := NewSessionCache(SessionCacheConfig{
		TTL:        time.Minute,
		MaxEntries: 2,
		Now:        func() time.Time { return now },
	})

	s1, _ := pairEphemeralForTest(t)
	s2, _ := pairEphemeralForTest(t)
	s3, _ := pairEphemeralForTest(t)

	if err := cache.Put("s1", s1, 0, "agent_up"); err != nil {
		t.Fatal(err)
	}
	now = now.Add(5 * time.Second)
	if err := cache.Put("s2", s2, 10, "agent_up"); err != nil {
		t.Fatal(err)
	}
	// 访问 s1 使 s2 成为最近最少使用项。
	now = now.Add(5 * time.Second)
	if got, sid, role, ok := cache.Get("s1"); !ok || got != s1 || sid != 0 || role != "agent_up" {
		t.Fatalf("应当命中 s1：%v %d %q %v", got != nil, sid, role, ok)
	}
	// BindServer 首次绑定成功，绑定到其他 serverID 被拒。
	if err := cache.BindServer("s1", 7); err != nil {
		t.Fatal(err)
	}
	if err := cache.BindServer("s1", 7); err != nil {
		t.Fatalf("重复绑定相同 serverID 应当幂等成功：%v", err)
	}
	if err := cache.BindServer("s1", 8); err == nil {
		t.Fatal("不能改绑到另一台服务器")
	}

	// 插入 s3 超出容量 2，应驱逐最久未用的 s2，保留 s1。
	now = now.Add(5 * time.Second)
	if err := cache.Put("s3", s3, 7, "mgmt"); err != nil {
		t.Fatal(err)
	}
	if _, _, _, ok := cache.Get("s2"); ok {
		t.Fatal("s2 应当因超出容量被驱逐")
	}
	if _, sid, _, ok := cache.Get("s1"); !ok || sid != 7 {
		t.Fatal("s1 应当保留且绑定 serverID=7")
	}
	if cache.Len() != 2 {
		t.Fatalf("当前有效会话应为 2：%d", cache.Len())
	}

	// 按角色和按服务器清理。
	cache.DeleteByServerRole(7, "mgmt")
	if _, _, _, ok := cache.Get("s3"); ok {
		t.Fatal("s3 应当按角色被清理")
	}
	if _, _, _, ok := cache.Get("s1"); !ok {
		t.Fatal("不同角色的 s1 不应当被清理")
	}
	cache.DeleteByServer(7)
	if cache.Len() != 0 {
		t.Fatalf("DeleteByServer 后应当为空：%d", cache.Len())
	}

	// 过期清理。
	if err := cache.Put("s4", s1, 1, "poll"); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Minute)
	if _, _, _, ok := cache.Get("s4"); ok {
		t.Fatal("超过 TTL 的会话应当过期失效")
	}
	if err := cache.BindServer("s4", 1); err == nil {
		t.Fatal("过期会话不能 BindServer")
	}
}

func TestSilentAuthAndNonceWindow(t *testing.T) {
	sig := SignSilentAuth("secret-tok-1", "POST", "/rpc", "nonce-1")
	if sig == "" || SignSilentAuth("", "POST", "/rpc", "nonce-1") != "" {
		t.Fatalf("空 token 不应当签出非空签名，非空 token 应当签出签名：%q", sig)
	}
	if matched, ok := VerifySilentAuth([]string{"secret-tok-2", "secret-tok-1"}, "POST", "/rpc", "nonce-1", sig); !ok || matched != "secret-tok-1" {
		t.Fatalf("双候选令牌应当匹配到 secret-tok-1：%q %v", matched, ok)
	}
	if _, ok := VerifySilentAuth([]string{""}, "POST", "/rpc", "nonce-1", sig); ok {
		t.Fatal("空令牌绝不能放行")
	}
	if _, ok := VerifySilentAuth([]string{"secret-tok-1"}, "GET", "/rpc", "nonce-1", sig); ok {
		t.Fatal("篡改 method 应当失败")
	}
	if _, ok := VerifySilentAuth([]string{"secret-tok-1"}, "POST", "/health", "nonce-1", sig); ok {
		t.Fatal("篡改 path 应当失败")
	}
	if _, ok := VerifySilentAuth([]string{"secret-tok-1"}, "POST", "/rpc", "nonce-2", sig); ok {
		t.Fatal("篡改 nonce 应当失败")
	}
	if _, ok := VerifySilentAuth([]string{"secret-tok-1"}, "POST", "/rpc", "nonce-1", ""); ok {
		t.Fatal("空签名应当失败")
	}

	now := time.Date(2026, 11, 18, 0, 0, 0, 0, time.UTC)
	nw := NewNonceWindow(time.Minute, 2, func() time.Time { return now })
	if nw.Allow("") {
		t.Fatal("空 nonce 应当拒绝")
	}
	if !nw.Allow("n1") || nw.Allow("n1") {
		t.Fatal("首次应当放行、窗口内重放应当拒绝")
	}
	if !nw.Allow("n2") || !nw.Allow("n3") {
		t.Fatal("新 nonce 应当放行")
	}
	now = now.Add(time.Minute + time.Second)
	if !nw.Allow("n2") {
		t.Fatal("窗口过期后同一 nonce 可以复用")
	}
}

func pairEphemeralForTest(t *testing.T) (*Session, *Session) {
	t.Helper()
	return pair(t)
}
