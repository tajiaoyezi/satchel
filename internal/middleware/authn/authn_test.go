package authn

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/user"
	"path/filepath"
	"testing"

	v1 "github.com/satchel/satchel/pkg/api/v1"
)

// whoamiHandler 把 ctx 里的身份写成 JSON。
func whoamiHandler() http.Handler {
	return Middleware(nil, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(v1.IdentityFrom(r.Context()))
	}))
}

// shortTempDir 给 unix socket 用：macOS 上 socket 路径不能超过 104 字节，t.TempDir() 的路径可能太长。
func shortTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "s")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

// startUnix 在临时 socket 上起一个带 ConnContext 的服务器，返回能连它的客户端。
func startUnix(t *testing.T, h http.Handler) *http.Client {
	t.Helper()
	path := filepath.Join(shortTempDir(t), "s.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: h, ConnContext: ConnContext}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	return &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", path)
	}}}
}

func fetch(t *testing.T, c *http.Client, url string, header http.Header) v1.Identity {
	t.Helper()
	req, _ := http.NewRequest("GET", url, nil)
	for k, v := range header {
		req.Header[k] = v
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var id v1.Identity
	if err := json.NewDecoder(resp.Body).Decode(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

// master-identity-and-authz「本机管理员的判定」：同 uid 经 socket 是 local_admin。
func TestUnixSocketPeerIsLocalAdmin(t *testing.T) {
	c := startUnix(t, whoamiHandler())
	id := fetch(t, c, "http://unix/whoami", nil)
	if id.ActorKind != v1.ActorLocalAdmin || id.Role != v1.RoleAdmin {
		t.Fatalf("经 socket 的同 uid 进程应当是 local_admin：%+v", id)
	}
	if u, err := user.Current(); err == nil && id.Actor != u.Username {
		t.Fatalf("actor 应当是 OS 用户名 %s，得到 %s", u.Username, id.Actor)
	}
	if !id.HasScope(v1.ScopeSecrets) || len(id.Danger) != len(v1.AllDangers) {
		t.Fatalf("本机管理员应当有全部 scope 与危险类：%+v", id)
	}
}

// TCP 上没有身份，伪造请求头也没用。
func TestTCPIsAnonymous(t *testing.T) {
	srv := httptest.NewUnstartedServer(whoamiHandler())
	srv.Config.ConnContext = ConnContext
	srv.Start()
	defer srv.Close()
	id := fetch(t, srv.Client(), srv.URL+"/whoami", http.Header{"X-Satchel-Actor": {"root"}})
	if !id.IsAnonymous() || id.Actor != "" || len(id.Scopes) != 0 {
		t.Fatalf("TCP 应当是 anonymous、请求头被忽略：%+v", id)
	}
}

func TestActorNameFallsBackToUID(t *testing.T) {
	if got := actorName(4000000000); got != "uid:4000000000" {
		t.Fatalf("查不到的 uid 应当写成 uid:<n>，得到 %s", got)
	}
}
