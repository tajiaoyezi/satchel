package auth

import (
	"context"
	"strings"
	"sync"

	"github.com/satchel/satchel/internal/base/captcha"
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
