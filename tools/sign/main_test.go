package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/satchel/satchel/pkg/release"
)

func capture(t *testing.T) (*os.File, func() string) {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "out")
	if err != nil {
		t.Fatal(err)
	}
	return f, func() string { data, _ := os.ReadFile(f.Name()); return string(data) }
}

func TestSignVerifyRoundTrip(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	saved := release.PublicKeys
	release.PublicKeys = []string{base64.StdEncoding.EncodeToString(pub)}
	t.Cleanup(func() { release.PublicKeys = saved })

	dir := t.TempDir()
	bin := filepath.Join(dir, "satchel-linux-amd64")
	if err := os.WriteFile(bin, []byte("binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	out, read := capture(t)
	privB64 := base64.StdEncoding.EncodeToString(priv)
	if code := run([]string{"sign", bin}, out, out, privB64); code != 0 {
		t.Fatalf("sign 退出码 %d：%s", code, read())
	}
	sig, err := os.ReadFile(bin + ".sig")
	if err != nil || len(sig) != release.SignatureSize {
		t.Fatalf(".sig 应当是 %d 字节，得到 %d（%v）", release.SignatureSize, len(sig), err)
	}
	if code := run([]string{"verify", bin, bin + ".sig"}, out, out, ""); code != 0 {
		t.Fatalf("verify 应当通过，退出码 %d：%s", code, read())
	}
	if err := os.WriteFile(bin, []byte("tampered"), 0o755); err != nil {
		t.Fatal(err)
	}
	if code := run([]string{"verify", bin, bin + ".sig"}, out, out, ""); code != 1 {
		t.Fatalf("篡改后 verify 应当退出 1，得到 %d", code)
	}
}

func TestSignRequiresEnvAndRejectsBadKey(t *testing.T) {
	out, read := capture(t)
	bin := filepath.Join(t.TempDir(), "f")
	os.WriteFile(bin, []byte("x"), 0o644)
	if code := run([]string{"sign", bin}, out, out, ""); code != 1 || !strings.Contains(read(), "RELEASE_SIGNING_PRIVATE_KEY 未设置") {
		t.Fatalf("没设环境变量应当退出 1 并点名：%d %s", code, read())
	}
	if code := run([]string{"sign", bin}, out, out, "bm90LWEta2V5"); code != 1 {
		t.Fatalf("坏私钥应当退出 1，得到 %d", code)
	}
	if _, err := os.Stat(bin + ".sig"); err == nil {
		t.Fatal("失败时不该留下 .sig")
	}
}

func TestKeygenAndUsage(t *testing.T) {
	out, read := capture(t)
	if code := run([]string{"keygen"}, out, out, ""); code != 0 {
		t.Fatalf("keygen 退出码 %d", code)
	}
	text := read()
	if !strings.Contains(text, "private (RELEASE_SIGNING_PRIVATE_KEY)") || !strings.Contains(text, "public (pkg/release.PublicKeys)") {
		t.Fatalf("keygen 输出形状不对：%s", text)
	}
	for _, args := range [][]string{nil, {"bogus"}, {"sign"}, {"verify", "a"}, {"keygen", "extra"}} {
		if code := run(args, out, out, ""); code != 2 {
			t.Errorf("%v 应当按用法错误退出 2，得到 %d", args, code)
		}
	}
	// 没有传私钥的命令行参数：--key 只会被当成文件名，读不到就失败，私钥不会从这里进来。
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	if code := run([]string{"sign", "--key", "x"}, out, out, base64.StdEncoding.EncodeToString(priv)); code != 1 || !strings.Contains(read(), "读取 --key 失败") {
		t.Fatalf("--key 不该是参数：%d %s", code, read())
	}
}
