package cli

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"

	v1 "github.com/satchel/satchel/pkg/api/v1"
	"github.com/satchel/satchel/pkg/release"
)

// release-pipeline「Master verifies release signatures itself」与 master-cli 的隐藏子命令。
func TestHiddenVerifyCommand(t *testing.T) {
	stdout, _, code := run(t, "--help")
	if code != 0 || strings.Contains(stdout, "__verify") {
		t.Fatalf("__verify 不该出现在帮助里：\n%s", stdout)
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	saved := release.PublicKeys
	release.PublicKeys = []string{base64.StdEncoding.EncodeToString(pub)}
	t.Cleanup(func() { release.PublicKeys = saved })

	dir := t.TempDir()
	bin := filepath.Join(dir, "satchel-linux-amd64")
	if err := os.WriteFile(bin, []byte("release bytes"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bin+".sig", ed25519.Sign(priv, []byte("release bytes")), 0o644); err != nil {
		t.Fatal(err)
	}
	stdout, stderr, code := run(t, "__verify", bin, bin+".sig")
	if code != 0 || !strings.Contains(stdout, "验签通过") {
		t.Fatalf("正确签名应当退出 0：%d\n%s\n%s", code, stdout, stderr)
	}
	stdout, _, code = run(t, "__verify", bin, bin+".sig", "--json")
	if code != 0 || !strings.Contains(stdout, `"verified":true`) {
		t.Fatalf("--json 应当输出 verified：%d %s", code, stdout)
	}
	if err := os.WriteFile(bin, []byte("tampered bytes"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, stderr, code = run(t, "__verify", bin, bin+".sig", "--json")
	if code != v1.ExitFailure {
		t.Fatalf("篡改后应当退出 1，得到 %d", code)
	}
	if e := decodeError(t, stderr); e.Code != v1.CodeBadRequest || !strings.Contains(e.Reason, "不匹配") || e.Next == "" {
		t.Fatalf("验签失败的四字段不对：%+v", e)
	}
	if _, _, code := run(t, "__verify", bin); code != v1.ExitUsage {
		t.Fatalf("少参数应当是用法错误 2，得到 %d", code)
	}
}
