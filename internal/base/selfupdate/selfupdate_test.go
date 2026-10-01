package selfupdate

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCompare(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"0.1.0", "v0.1.0", 0},
		{"0.1.1", "0.1.0", 1},
		{"0.10.0", "0.9.9", 1},
		{"1.0.0-alpha.1", "1.0.0", -1},
		{"1.0.0-alpha.2", "1.0.0-alpha.10", -1},
		{"1.0.0-alpha", "1.0.0-alpha.1", -1},
		{"1.0.0-1", "1.0.0-alpha", -1},
		{"1.0.0-beta", "1.0.0-alpha.9", 1},
		{"1.0.0+build.5", "1.0.0", 0},
		{"dev", "0.0.1", -1},
	}
	for _, c := range cases {
		if got := Compare(c.a, c.b); got != c.want {
			t.Errorf("Compare(%q, %q) = %d，应当 %d", c.a, c.b, got, c.want)
		}
	}
	for _, bad := range []string{"dev", "", "1.0", "1.0.0-", "01.0.0", "1.0.0-a..b", "1.x.0"} {
		if Valid(bad) {
			t.Errorf("%q 不应当合法", bad)
		}
	}
}

// server 是假的 CDN 与 GitHub：files 里是路径到内容，status 里可以给某个路径指定状态码。
type server struct {
	*httptest.Server
	files map[string][]byte
	hits  map[string]int
}

func newServer(t *testing.T) *server {
	s := &server{files: map[string][]byte{}, hits: map[string]int{}}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.URL.Path
		if r.URL.RawQuery != "" {
			key += "?" + r.URL.RawQuery
		}
		s.hits[key]++
		data, ok := s.files[key]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Write(data)
	}))
	t.Cleanup(s.Close)
	return s
}

func sources(cdn, gh *server) Sources {
	src := Sources{Repo: "o/satchel", API: gh.URL + "/api", Web: gh.URL + "/web", Proxy: gh.URL + "/proxy/"}
	if cdn != nil {
		src.CDN = cdn.URL
	}
	return src
}

// master-self-update「CDN 关着时走 GitHub」「域名为空等于不走 CDN」，CDN 失败回退 GitHub，prerelease 取最大、跳过草稿。
func TestLatest(t *testing.T) {
	ctx := context.Background()
	cdn, gh := newServer(t), newServer(t)
	gh.files["/api/repos/o/satchel/releases/latest"] = []byte(`{"tag_name":"v0.1.1","html_url":"u","body":"n"}`)
	gh.files["/api/repos/o/satchel/releases?per_page=30"] = []byte(`[{"tag_name":"v0.1.1"},{"tag_name":"v0.2.0-beta.2"},{"tag_name":"v0.3.0","draft":true},{"tag_name":"nightly"},{"tag_name":"v0.2.0-beta.10","prerelease":true}]`)
	cdn.files["/satchel/channels/stable/version.json"] = []byte(`{"version":"v0.1.2","tag":"v0.1.2","notes":"c"}`)

	rel, st, err := Latest(ctx, sources(cdn, gh), ChannelStable, false)
	if err != nil || rel.Source != SourceGitHub || rel.Version != "0.1.1" || rel.Tag != "v0.1.1" || st.Used || !strings.Contains(st.Reason, "关闭") || cdn.hits["/satchel/channels/stable/version.json"] != 0 {
		t.Fatalf("CDN 关着应当走 GitHub、不请求 CDN：%+v %+v %v", rel, st, err)
	}
	rel, st, err = Latest(ctx, sources(nil, gh), ChannelStable, true)
	if err != nil || rel.Source != SourceGitHub || !strings.Contains(st.Reason, "域名") {
		t.Fatalf("域名为空应当走 GitHub：%+v %+v %v", rel, st, err)
	}
	rel, st, err = Latest(ctx, sources(cdn, gh), ChannelStable, true)
	if err != nil || rel.Source != SourceCDN || rel.Version != "0.1.2" || !st.Used {
		t.Fatalf("CDN 可用时用 CDN：%+v %+v %v", rel, st, err)
	}
	rel, _, err = Latest(ctx, sources(cdn, gh), ChannelPrerelease, true) // CDN 上只有 stable 的索引：prerelease 渠道也算正式版
	if err != nil || rel.Source != SourceCDN || rel.Version != "0.1.2" {
		t.Fatalf("CDN 上 prerelease 渠道应当也看 stable 的索引：%+v %v", rel, err)
	}
	cdn.files["/satchel/channels/prerelease/version.json"] = []byte(`{"version":"v0.1.2-rc.1"}`)
	if rel, _, _ = Latest(ctx, sources(cdn, gh), ChannelPrerelease, true); rel.Version != "0.1.2" {
		t.Fatalf("两个索引取大的：%+v", rel)
	}
	cdn.files["/satchel/channels/prerelease/version.json"] = []byte(`{"version":"v0.2.0-beta.1"}`)
	if rel, _, _ = Latest(ctx, sources(cdn, gh), ChannelPrerelease, true); rel.Version != "0.2.0-beta.1" {
		t.Fatalf("两个索引取大的：%+v", rel)
	}
	delete(cdn.files, "/satchel/channels/stable/version.json")
	delete(cdn.files, "/satchel/channels/prerelease/version.json")
	rel, st, err = Latest(ctx, sources(cdn, gh), ChannelPrerelease, true) // CDN 上什么都没有
	if err != nil || rel.Source != SourceGitHub || rel.Version != "0.2.0-beta.10" || !strings.Contains(st.Reason, "不可用") {
		t.Fatalf("prerelease 应当回退 GitHub、取最大的非草稿：%+v %+v %v", rel, st, err)
	}
	// 审查：stable 的索引指向预发布版本时不认，回退 GitHub。
	cdn.files["/satchel/channels/stable/version.json"] = []byte(`{"version":"v0.3.0-rc.1"}`)
	if rel, st, _ := Latest(ctx, sources(cdn, gh), ChannelStable, true); rel.Source != SourceGitHub || !strings.Contains(st.Reason, "预发布") {
		t.Fatalf("stable 索引指向预发布时应当回退 GitHub：%+v %+v", rel, st)
	}
	delete(gh.files, "/api/repos/o/satchel/releases/latest")
	if _, _, err := Latest(ctx, sources(nil, gh), ChannelStable, false); err == nil || !strings.Contains(err.Error(), "GitHub") {
		t.Fatalf("取不到时应当带原因：%v", err)
	}
}

type signer struct {
	pub  ed25519.PublicKey
	priv ed25519.PrivateKey
}

func newSigner(t *testing.T) signer {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return signer{pub, priv}
}

func (s signer) sign(data []byte) []byte { return ed25519.Sign(s.priv, data) }

func (s signer) verify(bin, sig string) error {
	b, _ := os.ReadFile(bin)
	g, _ := os.ReadFile(sig)
	if !ed25519.Verify(s.pub, b, g) {
		return errors.New("签名不匹配")
	}
	return nil
}

// master-self-update「不同源的二进制与签名不配对」「三个源都只坏一样」。
func TestDownloadPairs(t *testing.T) {
	ctx := context.Background()
	key := newSigner(t)
	good, bad := []byte("new binary"), []byte("evil binary")
	rel := &Release{Version: "0.1.1", Tag: "v0.1.1"}
	name := BinaryName("amd64")
	cdnPath := "/satchel/releases/0.1.1/" + name
	ghPath := "/web/o/satchel/releases/download/v0.1.1/" + name
	proxyPath := "/proxy/" // gh-proxy 把 GitHub 的完整地址接在前缀后面

	t.Run("CDN 的二进制被换掉就整对换 GitHub", func(t *testing.T) {
		cdn, gh := newServer(t), newServer(t)
		cdn.files[cdnPath], cdn.files[cdnPath+".sig"] = bad, key.sign(good)
		gh.files[ghPath], gh.files[ghPath+".sig"] = good, key.sign(good)
		dir := t.TempDir()
		var seen []string
		a, err := Download(ctx, sources(cdn, gh), rel, true, "amd64", dir, key.verify, func(src string, n, total int64) { seen = append(seen, src) })
		if err != nil || a.Source != SourceGitHub {
			t.Fatalf("应当用 GitHub 的一对：%+v %v", a, err)
		}
		if data, _ := os.ReadFile(a.Binary); string(data) != string(good) || len(seen) == 0 {
			t.Fatalf("下载的内容不对：%q，进度 %v", data, seen)
		}
		if entries, _ := os.ReadDir(dir); len(entries) != 2 {
			t.Fatalf("目录里应当只剩这一对：%d", len(entries))
		}
	})

	t.Run("三个源各坏一样", func(t *testing.T) {
		cdn, gh := newServer(t), newServer(t)
		cdn.files[cdnPath], cdn.files[cdnPath+".sig"] = bad, key.sign(good) // 只投毒二进制
		gh.files[ghPath], gh.files[ghPath+".sig"] = good, key.sign(bad)     // 只投毒签名
		// gh-proxy：二进制与签名不成对（签的是另一个版本）
		full := gh.URL + ghPath
		gh.files[proxyPath+full] = good
		gh.files[proxyPath+full+".sig"] = key.sign([]byte("0.1.0 binary"))
		dir := t.TempDir()
		_, err := Download(ctx, sources(cdn, gh), rel, true, "amd64", dir, key.verify, nil)
		if err == nil || !strings.Contains(err.Error(), SourceCDN) || !strings.Contains(err.Error(), SourceGitHub) || !strings.Contains(err.Error(), SourceProxy) {
			t.Fatalf("应当失败并点名每个源：%v", err)
		}
		if gh.hits[proxyPath+full] != 1 {
			t.Fatal("三个源都应当试过")
		}
		if entries, _ := os.ReadDir(dir); len(entries) != 0 {
			t.Fatalf("失败后目录里不应当留下临时文件：%d", len(entries))
		}
	})

	t.Run("CDN 关着就不问 CDN", func(t *testing.T) {
		cdn, gh := newServer(t), newServer(t)
		gh.files[ghPath], gh.files[ghPath+".sig"] = good, key.sign(good)
		a, err := Download(ctx, sources(cdn, gh), rel, false, "amd64", t.TempDir(), key.verify, nil)
		if err != nil || a.Source != SourceGitHub || cdn.hits[cdnPath] != 0 {
			t.Fatalf("%+v %v", a, err)
		}
	})
}

func TestMarkerAndInstall(t *testing.T) {
	dataDir := t.TempDir()
	if m, err := ReadMarker(dataDir); m != nil || err != nil {
		t.Fatalf("没有标记时返回 nil：%+v %v", m, err)
	}
	want := &Marker{JobID: "job-1", FromVersion: "0.1.0", ToVersion: "0.1.1", Phase: PhaseSwitching, RequestedAt: time.Now().UTC().Truncate(time.Second)}
	if err := WriteMarker(dataDir, want); err != nil {
		t.Fatal(err)
	}
	got, err := ReadMarker(dataDir)
	if err != nil || got.JobID != "job-1" || got.Phase != PhaseSwitching || !got.RequestedAt.Equal(want.RequestedAt) {
		t.Fatalf("读回：%+v %v", got, err)
	}
	if fi, _ := os.Stat(markerPath(dataDir)); fi.Mode().Perm() != 0o600 {
		t.Fatalf("标记应当 0600：%v", fi.Mode())
	}
	if err := RemoveMarker(dataDir); err != nil {
		t.Fatal(err)
	}
	if m, _ := ReadMarker(dataDir); m != nil {
		t.Fatal("删掉后应当没有")
	}

	bin := t.TempDir()
	target := filepath.Join(bin, "satchel")
	os.WriteFile(target, []byte("old"), 0o755)
	staged := filepath.Join(bin, ".satchel-update-x")
	os.WriteFile(staged, []byte("new"), 0o600)
	if err := SavePrevious(target); err != nil {
		t.Fatal(err)
	}
	if replaced, err := Install(staged, target); err != nil || !replaced {
		t.Fatal(err)
	}
	if cur, prev := read(t, target), read(t, PreviousPath(target)); cur != "new" || prev != "old" {
		t.Fatalf("替换后：%q .bak：%q", cur, prev)
	}
	if fi, _ := os.Stat(target); fi.Mode().Perm() != 0o755 {
		t.Fatalf("新二进制应当可执行：%v", fi.Mode())
	}
	if err := RestorePrevious(target); err != nil {
		t.Fatal(err)
	}
	if cur, prev := read(t, target), read(t, PreviousPath(target)); cur != "old" || prev != "old" {
		t.Fatalf("放回后：%q .bak：%q", cur, prev)
	}
	if entries, _ := os.ReadDir(bin); len(entries) != 2 {
		t.Fatalf("不应当留下临时文件：%d", len(entries))
	}
}

func read(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestInDocker(t *testing.T) {
	root := t.TempDir()
	if InDocker(root) {
		t.Fatal("空目录不是 Docker")
	}
	os.MkdirAll(filepath.Join(root, "proc", "1"), 0o755)
	os.WriteFile(filepath.Join(root, "proc", "1", "cgroup"), []byte("0::/system.slice/satchel.service\n"), 0o644)
	if InDocker(root) {
		t.Fatal("普通的 cgroup 不是 Docker")
	}
	os.WriteFile(filepath.Join(root, "proc", "1", "cgroup"), []byte("0::/docker/abc\n"), 0o644)
	if !InDocker(root) {
		t.Fatal("cgroup 含 docker 应当是")
	}
	os.Remove(filepath.Join(root, "proc", "1", "cgroup"))
	os.WriteFile(filepath.Join(root, ".dockerenv"), nil, 0o644)
	if !InDocker(root) {
		t.Fatal("有 /.dockerenv 应当是")
	}
}
