package authn

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	v1 "github.com/satchel/satchel/pkg/api/v1"
)

// fakeProbes 记下每一次令牌校验失败。
type fakeProbes struct{ got []string }

func (f *fakeProbes) RecordProbe(_ context.Context, ip, path string) {
	f.got = append(f.got, ip+" "+path)
}

// master-login-protection「令牌猜测的计数与自动封禁」：经 TCP 带了无效凭据才计一次，按门放进 ctx 的来源 IP 与请求路径。
func TestProbeRecording(t *testing.T) {
	var asked []string
	probes := &fakeProbes{}
	h := Middleware(nil, fakeTokens{token: "sat_good", id: v1.Identity{Actor: "admin", ActorKind: v1.ActorToken}, asked: &asked}, probes,
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	serve := func(auth string, socket bool) {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/whoami", nil)
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		ctx := v1.WithRemote(req.Context(), v1.Remote{IP: "198.51.100.7"})
		if socket {
			ctx = MarkSocket(ctx)
		}
		h.ServeHTTP(httptest.NewRecorder(), req.WithContext(ctx))
	}
	serve("Bearer sat_nosuch", false)
	serve("Basic YWRtaW46eA==", false)
	serve("Bearer sat_good", false)
	serve("", false)
	serve("Bearer sat_nosuch", true)
	serve("Bearer sat_dberror", false) // 查库出错：照样拒绝，但不是猜令牌，不计
	if len(probes.got) != 2 || probes.got[0] != "198.51.100.7 /api/v1/whoami" || probes.got[1] != "198.51.100.7 /api/v1/whoami" {
		t.Fatalf("只有经 TCP 的无效令牌与非 Bearer 头各计一次：%v", probes.got)
	}
}
