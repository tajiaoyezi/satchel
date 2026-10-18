package auth

import (
	"context"
	"strings"
	"testing"

	"github.com/uptrace/bun"

	"github.com/satchel/satchel/internal/base/captcha"
	"github.com/satchel/satchel/internal/base/db/dbtest"
	"github.com/satchel/satchel/internal/command"
	coresecurity "github.com/satchel/satchel/internal/core/security"
	v1 "github.com/satchel/satchel/pkg/api/v1"
)

// fakeCaptcha 记下每次核对的参数，按 pass / err 回应。
type fakeCaptcha struct {
	calls  []string
	pass   bool
	codes  []string
	err    error
	secret string
	ip     string
}

func (f *fakeCaptcha) Verify(_ context.Context, secret, token, remoteIP string) (captcha.Result, error) {
	f.calls = append(f.calls, token)
	f.secret, f.ip = secret, remoteIP
	if f.err != nil {
		return captcha.Result{}, f.err
	}
	return captcha.Result{Success: f.pass, ErrorCodes: f.codes, Hostname: "panel.example.com"}, nil
}

func withCaptcha(t *testing.T, bdb *bun.DB) (*Service, *fakeCaptcha) {
	t.Helper()
	s, _ := limited(t, bdb)
	f := &fakeCaptcha{pass: true}
	s.SetCaptcha(f)
	s.SetTurnstileKeys("0x4AAAAAAAsitekeyforsatchel", "0x4AAAAAAAsecretforsatchel")
	return s, f
}

// master-login-protection「Turnstile 登录验证码」：没启用时照常登录，验证码配置不启用。
func TestTurnstileDisabled(t *testing.T) {
	dbtest.ForEach(t, func(t *testing.T, bdb *bun.DB) {
		s, f := withCaptcha(t, bdb)
		seedUser(t, bdb, "admin", "admin", true)
		s.SetTurnstileKeys("0x4AAAAAAAsitekeyforsatchel", "") // 只填 site key
		if _, err := s.Login(from("198.51.100.7"), "admin", password, false, ""); err != nil {
			t.Fatalf("没启用时不带验证码照常登录：%v", err)
		}
		if cfg := s.CaptchaConfig(context.Background()); cfg.Enabled || cfg.SiteKey != "" {
			t.Fatalf("只填一个 key 不算启用：%+v", cfg)
		}
		if len(f.calls) != 0 {
			t.Fatal("没启用不该调验证服务")
		}
	})
}

func TestTurnstileEnabled(t *testing.T) {
	dbtest.ForEach(t, func(t *testing.T, bdb *bun.DB) {
		s, f := withCaptcha(t, bdb)
		seedUser(t, bdb, "admin", "admin", true)
		ctx := from("198.51.100.7")
		if cfg := s.CaptchaConfig(context.Background()); !cfg.Enabled || cfg.SiteKey != "0x4AAAAAAAsitekeyforsatchel" {
			t.Fatalf("两个 key 都填了应当启用并给 site key：%+v", cfg)
		}
		// 没带 token：bad_request，点名 turnstile_token，不调验证服务、不计失败。
		_, err := s.Login(ctx, "admin", password, false, "")
		if e := v1.AsError(err); e.Code != v1.CodeBadRequest || !strings.Contains(e.Reason, "turnstile_token") {
			t.Fatalf("没带 token 应当 bad_request 并点名 turnstile_token：%v", err)
		}
		// 验证服务判为不通过：bad_request，state 带错误码。
		f.pass, f.codes = false, []string{"invalid-input-response"}
		_, err = s.Login(ctx, "admin", password, false, "tok-bad")
		e := v1.AsError(err)
		if e.Code != v1.CodeBadRequest || !strings.Contains(e.Reason, "turnstile_token") {
			t.Fatalf("不通过应当 bad_request：%v", err)
		}
		if codes, _ := e.State["error_codes"].([]string); len(codes) != 1 || codes[0] != "invalid-input-response" {
			t.Fatalf("state 应当带 Cloudflare 的错误码：%v", e.State)
		}
		if f.secret != "0x4AAAAAAAsecretforsatchel" || f.ip != "198.51.100.7" {
			t.Fatalf("核对时应当带 secret key 与来源 IP：%q %q", f.secret, f.ip)
		}
		// 验证服务不可用：unavailable。
		f.pass, f.err = true, v1.New(v1.CodeUnavailable, "连不上")
		_, err = s.Login(ctx, "admin", password, false, "tok")
		if v1.AsError(err).Code != v1.CodeUnavailable {
			t.Fatalf("验证服务不可用应当 unavailable：%v", err)
		}
		// 通过：比对密码、登录成功。
		f.err = nil
		res, err := s.Login(ctx, "admin", password, false, "tok-good")
		if err != nil || res.Token == "" {
			t.Fatalf("验证码通过、密码对应当登录成功：%+v %v", res, err)
		}
		// 验证码的失败不计失败、不记事件。
		if n := len(securityEvents(t, bdb, coresecurity.EventFilter{})); n != 0 {
			t.Fatalf("验证码的问题不算猜密码，得到 %d 条事件", n)
		}
	})
}

// 次序：锁定期内不调验证服务；经 unix socket 的登录不要验证码。
func TestTurnstileOrderAndSocket(t *testing.T) {
	dbtest.ForEach(t, func(t *testing.T, bdb *bun.DB) {
		s, f := withCaptcha(t, bdb)
		seedUser(t, bdb, "admin", "admin", true)
		ctx := from("198.51.100.7")
		for i := 0; i < 5; i++ {
			_, _ = s.Login(ctx, "admin", "wrong", false, "tok")
		}
		calls := len(f.calls)
		_, err := s.Login(ctx, "admin", password, false, "tok")
		if v1.AsError(err).Code != v1.CodeRateLimited || len(f.calls) != calls {
			t.Fatalf("锁定期内应当 rate_limited 且不调验证服务：%v，调用 %d → %d", err, calls, len(f.calls))
		}
		f.pass = false
		if _, err := s.Login(overSocket(), "admin", password, false, ""); err != nil {
			t.Fatalf("经 unix socket 的登录不要验证码：%v", err)
		}
	})
}

// master-login-protection「测试配置」：通过与不通过都照样返回结果；用的是已保存的 secret；不写安全事件。
func TestTurnstileTestCommand(t *testing.T) {
	dbtest.ForEach(t, func(t *testing.T, bdb *bun.DB) {
		s, f := withCaptcha(t, bdb)
		admin := v1.WithIdentity(from("198.51.100.7"), v1.LocalAdmin("root"))
		run := func(ctx context.Context, response string) (TurnstileTest, error) {
			out, err := s.turnstileTest(ctx, &command.Invocation{Flags: map[string]any{"response": response}})
			if err != nil {
				return TurnstileTest{}, err
			}
			return out.(TurnstileTest), nil
		}
		res, err := run(admin, "tok-good")
		if err != nil || !res.Success || len(res.ErrorCodes) != 0 || res.ErrorCodes == nil || res.Hostname != "panel.example.com" {
			t.Fatalf("通过时：%+v %v", res, err)
		}
		if f.secret != "0x4AAAAAAAsecretforsatchel" || f.ip != "198.51.100.7" {
			t.Fatalf("应当用已保存的 secret 与来源 IP：%q %q", f.secret, f.ip)
		}
		f.pass, f.codes = false, []string{"invalid-input-response"}
		if res, err = run(admin, "tok-bad"); err != nil || res.Success || len(res.ErrorCodes) != 1 || res.ErrorCodes[0] != "invalid-input-response" {
			t.Fatalf("不通过时照样返回结果：%+v %v", res, err)
		}
		if n := len(securityEvents(t, bdb, coresecurity.EventFilter{})); n != 0 {
			t.Fatalf("测试配置不写安全事件：%d", n)
		}
		// 经 unix socket 不带来源 IP。
		if _, err = run(v1.WithIdentity(overSocket(), v1.LocalAdmin("root")), "tok"); err != nil || f.ip != "" {
			t.Fatalf("经 socket 不带 IP：%v %q", err, f.ip)
		}
	})
}

// master-login-protection「测试配置的前提与权限」：两个 key 没都填是 conflict；连不上是 unavailable；普通用户 forbidden；
// 没给 response 是 bad_request。
func TestTurnstileTestPreconditions(t *testing.T) {
	dbtest.ForEach(t, func(t *testing.T, bdb *bun.DB) {
		s, f := withCaptcha(t, bdb)
		admin := v1.WithIdentity(from("198.51.100.7"), v1.LocalAdmin("root"))
		inv := func(response string) *command.Invocation {
			return &command.Invocation{Flags: map[string]any{"response": response}}
		}
		user := v1.WithIdentity(from("198.51.100.7"), v1.Identity{Actor: "carol", ActorKind: v1.ActorUser, Role: v1.RoleUser, Scopes: []v1.Scope{v1.ScopeRead, v1.ScopeOperate}})
		if _, err := s.turnstileTest(user, inv("x")); v1.AsError(err).Code != v1.CodeForbidden {
			t.Fatalf("普通用户应当 forbidden：%v", err)
		}
		if _, err := s.turnstileTest(admin, inv("")); v1.AsError(err).Code != v1.CodeBadRequest {
			t.Fatalf("没给 response 应当 bad_request：%v", err)
		}
		s.SetTurnstileKeys("0x4AAAAAAAsitekeyforsatchel", "")
		if _, err := s.turnstileTest(admin, inv("x")); v1.AsError(err).Code != v1.CodeConflict {
			t.Fatalf("只填一个 key 应当 conflict：%v", err)
		}
		s.SetTurnstileKeys("0x4AAAAAAAsitekeyforsatchel", "0x4AAAAAAAsecretforsatchel")
		f.err = v1.New(v1.CodeUnavailable, "连不上")
		if _, err := s.turnstileTest(admin, inv("x")); v1.AsError(err).Code != v1.CodeUnavailable {
			t.Fatalf("连不上应当 unavailable：%v", err)
		}
	})
}
