package auth

import (
	"context"
	"strings"
	"sync"

	"github.com/satchel/satchel/internal/base/captcha"
	"github.com/satchel/satchel/internal/command"
	v1 "github.com/satchel/satchel/pkg/api/v1"
)

// CaptchaVerifier 是 Turnstile 的核对（base/captcha 实现，测试换成假的）。
type CaptchaVerifier interface {
	Verify(ctx context.Context, secret, token, remoteIP string) (captcha.Result, error)
}

// CaptchaConfig 是登录页在登录之前要知道的：验证码启用没有、site key 是什么（GET /api/v1/session/captcha 的输出）。
// 没有 secret key：它只给主控自己核对用。
type CaptchaConfig struct {
	Enabled bool   `json:"enabled"`
	SiteKey string `json:"site_key"`
}

// turnstile 是 Turnstile 的当前设置与核对客户端；两个 key 都非空、且接了客户端才算启用。
type turnstile struct {
	mu       sync.RWMutex
	verifier CaptchaVerifier
	siteKey  string
	secret   string
}

func (t *turnstile) snapshot() (CaptchaVerifier, string, string) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.verifier, t.siteKey, t.secret
}

// SetCaptcha 接上 Turnstile 的核对客户端（装配根接 base/captcha）。
func (s *Service) SetCaptcha(v CaptchaVerifier) {
	s.turnstile.mu.Lock()
	defer s.turnstile.mu.Unlock()
	s.turnstile.verifier = v
}

// SetTurnstileKeys 换上两个 key（装配根在启动与每次设置写之后调），下一次登录就按它们判定。
func (s *Service) SetTurnstileKeys(siteKey, secretKey string) {
	s.turnstile.mu.Lock()
	defer s.turnstile.mu.Unlock()
	s.turnstile.siteKey, s.turnstile.secret = strings.TrimSpace(siteKey), strings.TrimSpace(secretKey)
}

// CaptchaConfig 返回登录页要的验证码配置：两个 key 都填了才启用，启用时给 site key。
func (s *Service) CaptchaConfig(context.Context) CaptchaConfig {
	v, site, secret := s.turnstile.snapshot()
	if v == nil || site == "" || secret == "" {
		return CaptchaConfig{SiteKey: ""}
	}
	return CaptchaConfig{Enabled: true, SiteKey: site}
}

// checkCaptcha 在查过登录限流之后、比对密码之前核对验证码（master-login-protection「Turnstile 登录验证码」）：
// 没启用或经 unix socket 就放过；没带、或 Cloudflare 判为不通过是 bad_request（不计失败）；服务不可用是 unavailable。
func (s *Service) checkCaptcha(ctx context.Context, token string) error {
	r, ok := guarded(ctx)
	if !ok {
		return nil
	}
	v, site, secret := s.turnstile.snapshot()
	if v == nil || site == "" || secret == "" {
		return nil
	}
	token = strings.TrimSpace(token)
	if token == "" {
		return v1.New(v1.CodeBadRequest, "登录要先通过人机验证：请求里缺 turnstile_token").WithNext("在登录页完成验证码后再提交")
	}
	res, err := v.Verify(ctx, secret, token, r.IP)
	if err != nil {
		return err
	}
	if !res.Success {
		codes := res.ErrorCodes
		if codes == nil {
			codes = []string{}
		}
		return v1.New(v1.CodeBadRequest, "人机验证没有通过：turnstile_token 无效或已过期").WithState("error_codes", codes).
			WithNext("刷新登录页，重新完成验证码后再提交")
	}
	return nil
}

// TurnstileTest 是 settings turnstile test 的输出：核对通过没有、Cloudflare 给的错误码与签发 token 的站点。
type TurnstileTest struct {
	Success    bool     `json:"success"`
	ErrorCodes []string `json:"error_codes"`
	Hostname   string   `json:"hostname"`
}

// turnstileTest 是 Turnstile 的测试配置（master-login-protection）：用已保存的 secret 核对网页上组件给出的 token。
// 只对管理员开放；两个 key 没都填是 conflict；连不上 Cloudflare 是 unavailable。核对不通过照样成功返回结果；
// 它不改设置，不计登录失败，不写安全事件。
func (s *Service) turnstileTest(ctx context.Context, inv *command.Invocation) (any, error) {
	if !v1.IdentityFrom(ctx).IsAdmin() {
		return nil, v1.New(v1.CodeForbidden, "Turnstile 的测试配置只对管理员开放")
	}
	v, site, secret := s.turnstile.snapshot()
	if v == nil || site == "" || secret == "" {
		return nil, v1.New(v1.CodeConflict, "Turnstile 没有启用：turnstile_site_key 与 turnstile_secret_key 两个都填了才能测试").
			WithNext("先在主控本机执行 satchel settings gates set --set turnstile_site_key=<site key> --set turnstile_secret_key=<secret key> --resource-version <N> --verify-user <管理员账号> 保存两个 key，再测试")
	}
	token := strings.TrimSpace(inv.String("response", ""))
	if token == "" {
		return nil, v1.New(v1.CodeBadRequest, "缺 --response：要测试的是 Turnstile 组件拿到的 token").
			WithNext("在网页的系统设置页点「测试配置」，由页面上的组件拿到 token")
	}
	remote := v1.RemoteFrom(ctx)
	ip := ""
	if !remote.Socket {
		ip = remote.IP
	}
	res, err := v.Verify(ctx, secret, token, ip)
	if err != nil {
		return nil, err
	}
	codes := res.ErrorCodes
	if codes == nil {
		codes = []string{}
	}
	return TurnstileTest{Success: res.Success, ErrorCodes: codes, Hostname: res.Hostname}, nil
}
