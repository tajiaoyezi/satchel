// Package captcha 是基础设施层的人机验证：调 Cloudflare Turnstile 的 siteverify，核对登录页上验证码组件给出的 token
// （master-login-protection「Turnstile 登录验证码」）。它只管这一次 HTTP 调用；启用与否、两个 key 从哪来由业务层决定。
package captcha

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	v1 "github.com/satchel/satchel/pkg/api/v1"
)

// SiteVerifyURL 是 Cloudflare 公布的核对地址。
const SiteVerifyURL = "https://challenges.cloudflare.com/turnstile/v0/siteverify"

// Timeout 是一次核对的上限（照 mmwx）。
const Timeout = 10 * time.Second

// maxResponseBytes 是读回应的上限：siteverify 的回应只有几百字节。
const maxResponseBytes = 64 << 10

// Client 调 siteverify。URL 只在测试里改（指向 httptest 的服务），不是设置项。
type Client struct {
	URL  string
	HTTP *http.Client
}

// New 建一个指向 Cloudflare、超时 10 秒的客户端。
func New() *Client {
	return &Client{URL: SiteVerifyURL, HTTP: &http.Client{Timeout: Timeout}}
}

// Result 是一次核对的结果：Success 为假时 ErrorCodes 是 Cloudflare 给的原因（如 invalid-input-response）。
type Result struct {
	Success    bool
	ErrorCodes []string
}

// Verify 把 token 连同 secret 与来源 IP（为空时不带）交给 siteverify 核对。服务不可用（连不上、超时、不是 2xx、
// 回应不是预期的 JSON）时返回 unavailable 错误；核对本身的结论（通过或不通过）在 Result 里。
func (c *Client) Verify(ctx context.Context, secret, token, remoteIP string) (Result, error) {
	form := url.Values{"secret": {secret}, "response": {token}}
	if remoteIP != "" {
		form.Set("remoteip", remoteIP)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.URL, strings.NewReader(form.Encode()))
	if err != nil {
		return Result{}, unavailable(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return Result{}, unavailable(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return Result{}, unavailable(fmt.Errorf("siteverify 回了 HTTP %d", resp.StatusCode))
	}
	var body struct {
		Success    *bool    `json:"success"`
		ErrorCodes []string `json:"error-codes"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponseBytes)).Decode(&body); err != nil {
		return Result{}, unavailable(err)
	}
	if body.Success == nil {
		return Result{}, unavailable(fmt.Errorf("siteverify 的回应里没有 success"))
	}
	return Result{Success: *body.Success, ErrorCodes: body.ErrorCodes}, nil
}

func unavailable(cause error) *v1.Error {
	return v1.Wrap(v1.CodeUnavailable, "连不上 Cloudflare 的人机验证服务，暂时无法核对验证码", cause).
		WithNext("稍后重试；主控本机的 CLI 不受影响，可以用 satchel settings gates set 清掉 Turnstile 的 key 关掉验证码")
}
