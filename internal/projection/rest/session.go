package rest

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"time"

	"github.com/satchel/satchel/internal/command"
	"github.com/satchel/satchel/internal/middleware/authn"
	"github.com/satchel/satchel/internal/service/auth"
	v1 "github.com/satchel/satchel/pkg/api/v1"
)

// SessionAPI 是会话入口要的业务（service/auth 实现）。
type SessionAPI interface {
	Login(ctx context.Context, username, password string, rememberMe bool, turnstileToken string) (*auth.LoginResult, error)
	CompleteTwoFactor(ctx context.Context, pending, code string) (*auth.LoginResult, error)
	Logout(ctx context.Context, token string) error
	IssueSession(ctx context.Context, username string, rememberMe bool) (string, time.Time, error)
	CaptchaConfig(ctx context.Context) auth.CaptchaConfig
}

// 会话入口的路径：命令表之外仅有的四条路由（另一个是 healthz）。
const (
	SessionPath          = command.APIPrefix + "session"
	SessionTwoFactorPath = command.APIPrefix + "session/two-factor"
	SessionCaptchaPath   = command.APIPrefix + "session/captcha"
)

// mountSessionEndpoints 挂四个会话入口：登录、两步验证第二步、登出（master-web-session），以及登录页在登录前取验证码配置的
// GET /api/v1/session/captcha（master-login-protection：不看身份、不进审计，只有 enabled 与 site_key）。
func mountSessionEndpoints(mux *http.ServeMux, sessions SessionAPI) {
	mux.HandleFunc(SessionCaptchaPath, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			WriteError(w, notFound(r))
			return
		}
		WriteResult(w, sessions.CaptchaConfig(r.Context()))
	})
	mux.HandleFunc(SessionPath, func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			var body struct {
				Username       string `json:"username"`
				Password       string `json:"password"`
				RememberMe     bool   `json:"remember_me"`
				TurnstileToken string `json:"turnstile_token"`
			}
			if err := readJSON(r, &body); err != nil {
				WriteError(w, err)
				return
			}
			res, err := sessions.Login(r.Context(), body.Username, body.Password, body.RememberMe, body.TurnstileToken)
			if err != nil {
				WriteError(w, err)
				return
			}
			if res.Token != "" {
				setSessionCookie(w, r, res.Token, res.ExpiresAt)
			}
			WriteResult(w, res)
		case http.MethodDelete:
			token := ""
			if c, err := r.Cookie(authn.SessionCookie); err == nil {
				token = c.Value
			}
			if err := sessions.Logout(r.Context(), token); err != nil {
				WriteError(w, err)
				return
			}
			clearSessionCookie(w, r)
			WriteResult(w, map[string]bool{"logged_out": true})
		default:
			WriteError(w, notFound(r))
		}
	})
	mux.HandleFunc(SessionTwoFactorPath, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			WriteError(w, notFound(r))
			return
		}
		var body struct {
			Pending string `json:"pending"`
			Code    string `json:"code"`
		}
		if err := readJSON(r, &body); err != nil {
			WriteError(w, err)
			return
		}
		res, err := sessions.CompleteTwoFactor(r.Context(), body.Pending, body.Code)
		if err != nil {
			WriteError(w, err)
			return
		}
		setSessionCookie(w, r, res.Token, res.ExpiresAt)
		WriteResult(w, res)
	})
}

func readJSON(r *http.Request, into any) error {
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, MaxBodyBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(into); err != nil {
		return v1.Wrap(v1.CodeBadRequest, "请求体不是预期形状的 JSON 对象", err)
	}
	return nil
}

// setSessionCookie 下发会话 cookie：HttpOnly、SameSite=Strict、Path=/，请求经 HTTPS 到达时带 Secure——TLS 直连，
// 或经登记的反向代理且标了 X-Forwarded-Proto: https（门算好放在 ctx 里，master-access-gates「本机与经 HTTPS 到达」）。
func setSessionCookie(w http.ResponseWriter, r *http.Request, token string, expires time.Time) {
	http.SetCookie(w, &http.Cookie{
		Name: authn.SessionCookie, Value: token, Path: "/", Expires: expires,
		HttpOnly: true, SameSite: http.SameSiteStrictMode, Secure: overHTTPS(r),
	})
}

func clearSessionCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name: authn.SessionCookie, Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, SameSite: http.SameSiteStrictMode, Secure: overHTTPS(r),
	})
}

func overHTTPS(r *http.Request) bool {
	return r.TLS != nil || v1.RemoteFrom(r.Context()).HTTPS
}

// SameOrigin 是防 CSRF 的同源检查，套在 authn 之内、整棵路由之外（REST、/mcp、会话入口都在里面）：
// 身份来自会话 cookie、没有身份（登录入口、向导）或带了无效凭据的写请求，带 Origin 时其 host 必须等于请求的 Host；
// 没 Origin 但 Sec-Fetch-Site 不是 same-origin / none 也拒；两个头都没有的是非浏览器客户端，放行。
// 经 unix socket 与有效令牌来的请求不受影响。
func SameOrigin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		src := v1.CredentialSourceFrom(r.Context())
		if (src == v1.SourceSession || src == v1.SourceNone || src == v1.SourceInvalid) && !safeMethod(r.Method) {
			if origin := r.Header.Get("Origin"); origin != "" {
				u, err := url.Parse(origin)
				if err != nil || u.Host != r.Host {
					WriteError(w, v1.New(v1.CodeForbidden, "跨站请求被拒：Origin 与主控地址不一致").WithState("origin", origin).WithState("host", r.Host))
					return
				}
			} else if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" && site != "none" {
				WriteError(w, v1.New(v1.CodeForbidden, "跨站请求被拒：Sec-Fetch-Site 为 "+site))
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func safeMethod(m string) bool {
	return m == http.MethodGet || m == http.MethodHead || m == http.MethodOptions
}
