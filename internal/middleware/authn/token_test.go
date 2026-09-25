package authn

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	v1 "github.com/satchel/satchel/pkg/api/v1"
)

// fakeTokens 认一个固定令牌，并记下被问过的令牌。
type fakeTokens struct {
	token string
	id    v1.Identity
	asked *[]string
}

func (f fakeTokens) ResolveToken(_ context.Context, token string) (v1.Identity, bool, error) {
	*f.asked = append(*f.asked, token)
	if token == f.token {
		return f.id, true, nil
	}
	if token == "sat_dberror" {
		return v1.Identity{}, false, errors.New("数据库不可用")
	}
	return v1.Identity{}, false, nil
}

type request struct {
	auth   []string // Authorization 头的各个值
	cookie string
	socket bool // 模拟本进程 uid 经 socket 进来
}

func callWith(t *testing.T, h http.Handler, rq request) (v1.Identity, v1.CredentialSource) {
	t.Helper()
	req := httptest.NewRequest("GET", "/whoami", nil)
	for _, v := range rq.auth {
		req.Header.Add("Authorization", v)
	}
	if rq.cookie != "" {
		req.AddCookie(&http.Cookie{Name: SessionCookie, Value: rq.cookie})
	}
	if rq.socket {
		req = req.WithContext(context.WithValue(req.Context(), peerKey{}, peer{uid: uint32(os.Getuid())}))
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var out struct {
		Identity v1.Identity         `json:"identity"`
		Source   v1.CredentialSource `json:"source"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out.Identity, out.Source
}

// master-identity-and-authz「身份的判定顺序」：Authorization 头 → 令牌；socket；cookie；anonymous。
func TestTokenPrecedence(t *testing.T) {
	var asked []string
	tokID := int64(7)
	tokenIdentity := v1.Identity{Actor: "admin", ActorKind: v1.ActorToken, Role: v1.RoleAdmin, TokenID: &tokID, Scopes: []v1.Scope{v1.ScopeRead}, Danger: []v1.Danger{}}
	user := v1.Identity{Actor: "alice", ActorKind: v1.ActorUser, Role: v1.RoleUser, Scopes: []v1.Scope{v1.ScopeRead, v1.ScopeOperate}, Danger: []v1.Danger{}}
	h := Middleware(fakeResolver{token: "session", id: user}, fakeTokens{token: "sat_good", id: tokenIdentity, asked: &asked}, nil, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"identity": v1.IdentityFrom(r.Context()), "source": v1.CredentialSourceFrom(r.Context())})
	}))

	valid := []request{
		{auth: []string{"Bearer sat_good"}},
		{auth: []string{"Bearer sat_good"}, socket: true},                    // 本机进程带令牌只有令牌的权限
		{auth: []string{"Bearer sat_good"}, cookie: "session"},               // 令牌优先于会话
		{auth: []string{"bearer   sat_good  "}},                              // 方案不分大小写，前后空白不算
		{auth: []string{"BEARER sat_good"}, socket: true, cookie: "session"}, // 三样都带，按令牌算
	}
	for _, rq := range valid {
		id, src := callWith(t, h, rq)
		if id.ActorKind != v1.ActorToken || id.TokenID == nil || *id.TokenID != 7 || src != v1.SourceToken {
			t.Errorf("%+v 应当是令牌身份：%+v %s", rq, id, src)
		}
	}

	invalid := []request{
		{auth: []string{"Bearer sat_nosuch"}},
		{auth: []string{"Bearer sat_nosuch"}, socket: true},      // 无效令牌不退回本机管理员
		{auth: []string{"Bearer sat_nosuch"}, cookie: "session"}, // 也不退回会话
		{auth: []string{"Basic YWRtaW46eA=="}},
		{auth: []string{"Basic YWRtaW46eA=="}, socket: true},
		{auth: []string{"Bearer"}},
		{auth: []string{"Bearer "}},
		{auth: []string{""}},
		{auth: []string{"Bearer a b"}},
		{auth: []string{"sat_good"}},
		{auth: []string{"Bearer sat_good", "Bearer sat_good"}}, // 两个头不认
	}
	for _, rq := range invalid {
		id, src := callWith(t, h, rq)
		if !id.IsAnonymous() || id.Actor != "" || len(id.Scopes) != 0 || src != v1.SourceInvalid {
			t.Errorf("%+v 应当是无效凭据：%+v %s", rq, id, src)
		}
	}

	// 没有 Authorization 头时照旧：socket、cookie、anonymous。
	if id, src := callWith(t, h, request{socket: true, cookie: "session"}); id.ActorKind != v1.ActorLocalAdmin || src != v1.SourceSocket {
		t.Fatalf("socket 优先于 cookie：%+v %s", id, src)
	}
	if id, src := callWith(t, h, request{cookie: "session"}); id.ActorKind != v1.ActorUser || src != v1.SourceSession {
		t.Fatalf("会话：%+v %s", id, src)
	}
	if id, src := callWith(t, h, request{}); !id.IsAnonymous() || src != v1.SourceNone {
		t.Fatalf("什么都不带是 anonymous：%+v %s", id, src)
	}

	// 不是 Bearer 的头不去问令牌解析器；是 Bearer 的，原样交给它（前后空白去掉）。
	asked = nil
	callWith(t, h, request{auth: []string{"Basic YWRtaW46eA=="}})
	callWith(t, h, request{auth: []string{"bearer  sat_x "}})
	if len(asked) != 1 || asked[0] != "sat_x" {
		t.Fatalf("解析器被问到的令牌：%q", asked)
	}

	// 没有令牌解析器时，带 Authorization 头一律是无效凭据。
	noTokens := Middleware(nil, nil, nil, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"identity": v1.IdentityFrom(r.Context()), "source": v1.CredentialSourceFrom(r.Context())})
	}))
	if id, src := callWith(t, noTokens, request{auth: []string{"Bearer sat_good"}, socket: true}); !id.IsAnonymous() || src != v1.SourceInvalid {
		t.Fatalf("没有解析器：%+v %s", id, src)
	}
}
