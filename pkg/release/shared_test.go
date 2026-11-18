package release

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSemverAndCompatMeta(t *testing.T) {
	if !Valid("v0.2.0") || !Valid("0.2.0-beta.1") || Valid("dev") {
		t.Fatal("Semver 校验不符合预期")
	}
	if !IsDev("0.0.0-dev") || IsDev("0.2.0-beta.1") {
		t.Fatal("IsDev 判断不符合预期")
	}
	if Compare("0.2.0-beta.2", "0.2.0-beta.1") <= 0 || Compare("0.2.0", "0.2.0-beta.9") <= 0 {
		t.Fatal("Compare 排序不对")
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/cdn/satchel-agent/channels/stable/version.json":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"version":            "0.2.1",
				"tag":                "v0.2.1",
				"control_protocol":   2,
				"min_master_version": "0.2.5",
			})
		case "/api/repos/o/satchel-agent/releases/latest":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"tag_name": "v0.2.2",
				"body":     "notes\n<!-- satchel-meta: {\"control_protocol\":1,\"min_master_version\":\"0.2.0\"} -->",
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	src := Sources{CDN: srv.URL + "/cdn", Project: "satchel-agent", Repo: "o/satchel-agent", API: srv.URL + "/api"}
	rel, cdn, err := Latest(context.Background(), src, ChannelStable, true)
	if err != nil || !cdn.Used || rel.Version != "0.2.1" || rel.ControlProtocol != 2 || rel.MinMasterVersion != "0.2.5" {
		t.Fatalf("CDN agent 元数据不对：rel=%+v cdn=%+v err=%v", rel, cdn, err)
	}

	rel2, cdn2, err := Latest(context.Background(), src, ChannelStable, false)
	if err != nil || cdn2.Used || rel2.Version != "0.2.2" || rel2.ControlProtocol != 1 || rel2.MinMasterVersion != "0.2.0" {
		t.Fatalf("GitHub agent 元数据不对：rel=%+v cdn=%+v err=%v", rel2, cdn2, err)
	}
}

func TestDownloadPairThreeSourcePoisonAndFreeSpace(t *testing.T) {
	pubB64, priv := testKey(t)
	withKeys(t, pubB64)

	goodBin := []byte("agent-binary-good")
	goodSig := ed25519.Sign(priv, goodBin)
	badSig := ed25519.Sign(priv, []byte("other-binary"))

	mode := "cdn-poisoned-gh-ok"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch mode {
		case "cdn-poisoned-gh-ok":
			switch r.URL.Path {
			case "/cdn/satchel-agent/releases/0.2.0/satchel-agent-linux-amd64":
				_, _ = w.Write([]byte("tampered"))
			case "/cdn/satchel-agent/releases/0.2.0/satchel-agent-linux-amd64.sig":
				_, _ = w.Write(goodSig)
			case "/web/o/satchel-agent/releases/download/v0.2.0/satchel-agent-linux-amd64":
				_, _ = w.Write(goodBin)
			case "/web/o/satchel-agent/releases/download/v0.2.0/satchel-agent-linux-amd64.sig":
				_, _ = w.Write(goodSig)
			default:
				w.WriteHeader(http.StatusNotFound)
			}
		case "all-three-poisoned":
			switch {
			case strings.HasPrefix(r.URL.Path, "/cdn/") && strings.HasSuffix(r.URL.Path, ".sig"):
				_, _ = w.Write(goodSig)
			case strings.HasPrefix(r.URL.Path, "/cdn/"):
				_, _ = w.Write([]byte("bad-cdn-bin"))
			case strings.HasPrefix(r.URL.Path, "/web/") && strings.HasSuffix(r.URL.Path, ".sig"):
				_, _ = w.Write([]byte("bad-sig"))
			case strings.HasPrefix(r.URL.Path, "/web/"):
				_, _ = w.Write(goodBin)
			case strings.HasPrefix(r.URL.Path, "/proxy/") && strings.HasSuffix(r.URL.Path, ".sig"):
				_, _ = w.Write(badSig)
			case strings.HasPrefix(r.URL.Path, "/proxy/"):
				_, _ = w.Write(goodBin)
			}
		}
	}))
	defer srv.Close()

	src := Sources{
		CDN:     srv.URL + "/cdn",
		Project: "satchel-agent",
		Repo:    "o/satchel-agent",
		Web:     srv.URL + "/web",
		Proxy:   srv.URL + "/proxy/",
	}
	rel := &Release{Version: "0.2.0", Tag: "v0.2.0"}
	dir := t.TempDir()

	// 1. CDN 二进制与签名不匹配，禁止与 GitHub 签名混配，整组切 GitHub 成功。
	art, err := DownloadPair(context.Background(), src, rel, true, "amd64", dir, nil, nil)
	if err != nil || art.Source != SourceGitHub {
		t.Fatalf("跨源切换失败：art=%+v err=%v", art, err)
	}
	art.Remove()

	// 2. 三源分别投毒 bin、投毒 sig、bin/sig 不成对：全部失败且无残留临时文件。
	mode = "all-three-poisoned"
	if _, err := DownloadPair(context.Background(), src, rel, true, "amd64", dir, nil, nil); err == nil {
		t.Fatal("三源均投毒时应当全部失败")
	} else {
		for _, tag := range []string{"cdn：", "github：", "gh-proxy："} {
			if !strings.Contains(err.Error(), tag) {
				t.Fatalf("失败信息应当点名每个源（缺 %q）：%v", tag, err)
			}
		}
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("失败后目录不该残留临时文件：%v", entries)
	}

	// 3. 空间不足 128 MiB 被拒绝；实盘 Statfs 检查通过。
	if err := CheckFreeSpace(dir, MinStagingFreeBytes, func(string) (uint64, error) { return 64 << 20, nil }); err == nil {
		t.Fatal("可用空间仅 64 MiB 应当被拒绝")
	}
	if avail, err := FreeSpaceBytes(dir); err != nil || avail == 0 {
		t.Fatalf("FreeSpaceBytes 异常：avail=%d err=%v", avail, err)
	}

	// 4. 容器检测：.dockerenv 与 cgroup(containerd/docker)。
	root := t.TempDir()
	if InDocker(root) {
		t.Fatal("空根不应判在容器里")
	}
	_ = os.WriteFile(filepath.Join(root, ".dockerenv"), nil, 0o600)
	if !InDocker(root) {
		t.Fatal("存在 .dockerenv 应当判为容器")
	}
}
