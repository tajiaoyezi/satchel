package rest

import (
	"net/http"
	"strings"
)

// CORS 是跨域调用的处理（master-access-gates「跨域调用只给令牌用」）：origins 是 SATCHEL_ALLOWED_ORIGINS 归一后的来源列表，
// 或单独一个 *；为空时原样返回 next，不发任何跨域头（只允许同源）。请求的 Origin 在列表里时带三项跨域头，列表时另加
// Vary: Origin；这样的来源发来的 OPTIONS 预检直接 204、不进路由。任何情况下都不发 Access-Control-Allow-Credentials：
// 跨域只给带令牌的调用用，会话 cookie 不跨域，带 cookie 的跨站写请求照旧被同源检查拒绝。
func CORS(origins []string, next http.Handler) http.Handler {
	if len(origins) == 0 {
		return next
	}
	anyOrigin := len(origins) == 1 && origins[0] == "*"
	allowed := make(map[string]bool, len(origins))
	for _, o := range origins {
		allowed[o] = true
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin != "" && (anyOrigin || allowed[strings.ToLower(origin)]) {
			h := w.Header()
			if anyOrigin {
				h.Set("Access-Control-Allow-Origin", "*")
			} else {
				h.Set("Access-Control-Allow-Origin", origin)
				h.Add("Vary", "Origin")
			}
			h.Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
			h.Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
			if r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != "" {
				w.WriteHeader(http.StatusNoContent)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}
