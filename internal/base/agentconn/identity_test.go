package agentconn

import (
	"bytes"
	"crypto/ed25519"
	"os"
	"path/filepath"
	"testing"

	"github.com/satchel/satchel/internal/base/db"
	v1 "github.com/satchel/satchel/pkg/api/v1"
)

// node-channel「首次启动生成、重启不变」：空目录生成 0600 的 master.key，再读一次公钥不变。
func TestLoadIdentityGeneratesOnce(t *testing.T) {
	dir := t.TempDir()
	first, err := LoadIdentity(dir)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(dir, db.MasterKeyFile))
	if err != nil || info.Mode().Perm() != 0o600 || info.Size() != ed25519.SeedSize {
		t.Fatalf("应当生成 0600、32 字节的 master.key：%v %v", info, err)
	}
	second, err := LoadIdentity(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !first.Public().(ed25519.PublicKey).Equal(second.Public()) {
		t.Fatal("再读一次公钥应当不变")
	}
}

// node-channel「坏的密钥文件」：10 个字节的 master.key 报 config，文件内容不变。
func TestLoadIdentityRejectsBadFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, db.MasterKeyFile)
	bad := []byte("0123456789")
	if err := os.WriteFile(path, bad, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := LoadIdentity(dir)
	if e := v1.AsError(err); e.Code != v1.CodeConfig {
		t.Fatalf("应当是 config：%v", err)
	}
	if got, _ := os.ReadFile(path); !bytes.Equal(got, bad) {
		t.Fatal("坏的 master.key 不应当被改写")
	}
}
