package release

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testKey(t *testing.T) (string, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(pub), priv
}

// 用一组测试公钥替换清单，测试结束还原。
func withKeys(t *testing.T, keys ...string) {
	t.Helper()
	saved := PublicKeys
	PublicKeys = keys
	t.Cleanup(func() { PublicKeys = saved })
}

func TestVerifyAcceptsGoodSignatureRejectsTampered(t *testing.T) {
	pub, priv := testKey(t)
	withKeys(t, pub)
	data := []byte("satchel release payload")
	sig := ed25519.Sign(priv, data)
	if len(sig) != SignatureSize {
		t.Fatalf("签名应当 %d 字节，得到 %d", SignatureSize, len(sig))
	}
	if err := Verify(data, sig); err != nil {
		t.Fatalf("正确的签名应当通过：%v", err)
	}
	tampered := append([]byte(nil), data...)
	tampered[0] ^= 0x01
	if err := Verify(tampered, sig); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("篡改后应当报 ErrBadSignature，得到 %v", err)
	}
	if err := Verify(data, sig[:10]); err == nil || errors.Is(err, ErrBadSignature) {
		t.Fatalf("长度不对应当单独报出，得到 %v", err)
	}
}

func TestVerifyFile(t *testing.T) {
	pub, priv := testKey(t)
	withKeys(t, pub)
	dir := t.TempDir()
	path := filepath.Join(dir, "bin")
	data := []byte("binary bytes")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+".sig", ed25519.Sign(priv, data), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := VerifyFile(path, path+".sig"); err != nil {
		t.Fatalf("文件验签应当通过：%v", err)
	}
	if err := VerifyFile(filepath.Join(dir, "missing"), path+".sig"); err == nil {
		t.Fatal("文件不存在应当报错")
	}
}

// 轮换：清单里有新旧两把，用第二把（旧钥）签的也通过；空清单一律失败。
func TestRotationListAndEmptyList(t *testing.T) {
	newPub, _ := testKey(t)
	oldPub, oldPriv := testKey(t)
	withKeys(t, newPub, oldPub)
	data := []byte("signed by the old key")
	sig := ed25519.Sign(oldPriv, data)
	if err := Verify(data, sig); err != nil {
		t.Fatalf("旧钥在清单里就应当通过：%v", err)
	}
	withKeys(t)
	if err := Verify(data, sig); err == nil || errors.Is(err, ErrBadSignature) {
		t.Fatalf("空清单应当报「清单为空」，得到 %v", err)
	}
	withKeys(t, "not-base64!")
	if err := Verify(data, sig); err == nil || errors.Is(err, ErrBadSignature) {
		t.Fatalf("清单里的坏公钥应当报出而不是跳过，得到 %v", err)
	}
}

func TestSplitKeys(t *testing.T) {
	got := splitKeys(" a , ,b,")
	if len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Fatalf("splitKeys 应当去空白与空项，得到 %v", got)
	}
	if len(splitKeys("")) != 0 {
		t.Fatal("空字符串应当是空清单")
	}
}

// 占位公钥的形状要对（32 字节），否则 Verify 会在第一把上就报格式错。
func TestPlaceholderKeyShape(t *testing.T) {
	for _, k := range PublicKeys {
		if _, err := decodePublicKey(k); err != nil {
			t.Fatalf("清单里的公钥形状不对：%v", err)
		}
	}
}

// install.sh 内嵌的公钥清单必须与本包一致：安装脚本用 openssl 加脚本里的公钥验签（信任锚是脚本，不是刚下载的二进制），
// 两处不同步就会出现「脚本装不了新签的包」或「脚本认旧钥」。
func TestInstallScriptKeysMatch(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "install.sh"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	start := strings.Index(text, "# BEGIN RELEASE PUBLIC KEYS")
	end := strings.Index(text, "# END RELEASE PUBLIC KEYS")
	if start < 0 || end < 0 || end < start {
		t.Fatal("install.sh 里找不到公钥清单的标记")
	}
	var scriptKeys []string
	for _, line := range strings.Split(text[start:end], "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "RELEASE_PUBLIC_KEYS=") || line == `"` {
			continue
		}
		scriptKeys = append(scriptKeys, line)
	}
	if strings.Join(scriptKeys, ",") != strings.Join(PublicKeys, ",") {
		t.Fatalf("install.sh 的公钥清单 %v 与 pkg/release 的 %v 不一致", scriptKeys, PublicKeys)
	}
}
