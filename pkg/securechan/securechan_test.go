package securechan

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"testing"
)

// pair 建一对配好的会话：节点一侧与主控一侧。
func pair(t *testing.T) (agent, master *Session) {
	t.Helper()
	agentPriv, agentPub, err := GenerateEphemeral()
	if err != nil {
		t.Fatal(err)
	}
	masterPriv, masterPub, err := GenerateEphemeral()
	if err != nil {
		t.Fatal(err)
	}
	sharedA, err := ComputeSharedSecret(agentPriv, masterPub)
	if err != nil {
		t.Fatal(err)
	}
	sharedM, err := ComputeSharedSecret(masterPriv, agentPub)
	if err != nil {
		t.Fatal(err)
	}
	if agent, err = DeriveSession(sharedA, agentPub, masterPub, false); err != nil {
		t.Fatal(err)
	}
	if master, err = DeriveSession(sharedM, agentPub, masterPub, true); err != nil {
		t.Fatal(err)
	}
	return agent, master
}

func TestPublicKeyEncoding(t *testing.T) {
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	got, err := ParsePublicKey(EncodePublicKey(pub))
	if err != nil || !got.Equal(pub) {
		t.Fatalf("往返之后公钥不一致：%v", err)
	}
	for _, bad := range []string{"bad-base64!!!", EncodePublicKey(pub[:16]), ""} {
		if _, err := ParsePublicKey(bad); err == nil {
			t.Fatalf("%q 应当解析失败", bad)
		}
	}
	sum := sha256.Sum256(pub)
	if Fingerprint(pub) != hex.EncodeToString(sum[:]) || len(Fingerprint(pub)) != 64 {
		t.Fatalf("指纹应当是原始公钥的 SHA-256 十六进制：%s", Fingerprint(pub))
	}
}

func TestEphemeralAndECDH(t *testing.T) {
	aPriv, aPub, _ := GenerateEphemeral()
	bPriv, bPub, _ := GenerateEphemeral()
	s1, err := ComputeSharedSecret(aPriv, bPub)
	if err != nil {
		t.Fatal(err)
	}
	s2, err := ComputeSharedSecret(bPriv, aPub)
	if err != nil {
		t.Fatal(err)
	}
	if len(s1) != 32 || string(s1) != string(s2) {
		t.Fatal("双方算出的共享密钥应当相同")
	}
	if _, err := ComputeSharedSecret(aPriv, make([]byte, 32)); err == nil {
		t.Fatal("低阶点（全零公钥）应当报错")
	}
}

// 签名覆盖双方的临时公钥：换任何一把、换主控身份，签名都验不过；另一次握手的签名搬过来也验不过。
func TestHandshakeSignatureBindsBothKeys(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	_, agentEph, _ := GenerateEphemeral()
	_, masterEph, _ := GenerateEphemeral()
	sig := SignHandshake(priv, agentEph, masterEph)
	if !VerifyHandshake(pub, agentEph, masterEph, sig) {
		t.Fatal("正确的签名应当通过")
	}
	_, otherAgent, _ := GenerateEphemeral()
	_, otherMaster, _ := GenerateEphemeral()
	if VerifyHandshake(pub, otherAgent, masterEph, sig) {
		t.Fatal("换了节点临时公钥，签名应当验不过")
	}
	if VerifyHandshake(pub, agentEph, otherMaster, sig) {
		t.Fatal("换了主控临时公钥，签名应当验不过")
	}
	otherPub, _, _ := ed25519.GenerateKey(rand.Reader)
	if VerifyHandshake(otherPub, agentEph, masterEph, sig) {
		t.Fatal("别的主控公钥应当验不过")
	}
	// mmwx 只签主控临时公钥：那样的签名在这里不算数。
	if VerifyHandshake(pub, agentEph, masterEph, ed25519.Sign(priv, masterEph)) {
		t.Fatal("只签主控临时公钥的签名应当验不过")
	}
	if VerifyHandshake(pub, agentEph[:16], masterEph, sig) {
		t.Fatal("长度不对的临时公钥应当验不过")
	}
}

func TestEncryptDecryptRoundTrip(t *testing.T) {
	agent, master := pair(t)
	for i := 0; i < 100; i++ {
		msg := []byte{byte(i), 'x'}
		env, err := agent.Encrypt(msg)
		if err != nil {
			t.Fatal(err)
		}
		if env[0] != EnvelopeVersion || binary.BigEndian.Uint64(env[1:9]) != uint64(i+1) {
			t.Fatalf("第 %d 帧的版本或序号不对", i)
		}
		got, err := master.Decrypt(env)
		if err != nil || string(got) != string(msg) {
			t.Fatalf("第 %d 帧：%v", i, err)
		}
	}
	env, _ := master.Encrypt([]byte("down"))
	if got, err := agent.Decrypt(env); err != nil || string(got) != "down" {
		t.Fatalf("反方向：%v", err)
	}
}

func TestTamperAndWrongKey(t *testing.T) {
	agent, master := pair(t)
	env, _ := agent.Encrypt([]byte("secret"))
	env[len(env)-1] ^= 0xFF
	if _, err := master.Decrypt(env); err == nil {
		t.Fatal("篡改过的信封应当解密失败")
	}
	_, other := pair(t)
	env, _ = agent.Encrypt([]byte("secret"))
	if _, err := other.Decrypt(env); err == nil {
		t.Fatal("别的会话应当解不开")
	}
	if _, err := master.Decrypt([]byte{0x01, 0x00}); err == nil {
		t.Fatal("太短的信封应当报错")
	}
	env, _ = agent.Encrypt([]byte("v"))
	env[0] = 0xFF
	if _, err := master.Decrypt(env); err == nil {
		t.Fatal("不认识的版本应当报错")
	}
}

func TestReplayRejected(t *testing.T) {
	agent, master := pair(t)
	env, _ := agent.Encrypt([]byte("msg1"))
	if _, err := master.Decrypt(env); err != nil {
		t.Fatal(err)
	}
	if _, err := master.Decrypt(env); !errors.Is(err, ErrReplay) {
		t.Fatalf("重放应当被拒：%v", err)
	}
}

// 伪造一帧序号很大、标签错误的帧：解密失败，窗口不前移，之后正常序号的帧照常能解开（mmwx 在这里会把窗口推走）。
func TestForgedHighSeqDoesNotMoveWindow(t *testing.T) {
	agent, master := pair(t)
	forged, _ := agent.Encrypt([]byte("x"))
	binary.BigEndian.PutUint64(forged[1:9], 1_000_000)
	if _, err := master.Decrypt(forged); err == nil {
		t.Fatal("伪造的帧应当解密失败")
	}
	for i := 0; i < 3; i++ {
		env, _ := agent.Encrypt([]byte("ok"))
		if _, err := master.Decrypt(env); err != nil {
			t.Fatalf("伪造帧之后第 %d 帧应当照常解开：%v", i+1, err)
		}
	}
}

func TestReplayWindow(t *testing.T) {
	var w replayWindow
	accept := func(seq uint64) bool {
		if !w.fresh(seq) {
			return false
		}
		w.mark(seq)
		return true
	}
	if accept(0) {
		t.Fatal("序号 0 应当被拒")
	}
	if !accept(1) || accept(1) || !accept(2) {
		t.Fatal("1 收、1 重放拒、2 收")
	}
	if !accept(100) {
		t.Fatal("跳到 100 应当收")
	}
	if !accept(100 - windowSize + 1) {
		t.Fatalf("%d 还在窗口里，应当收", 100-windowSize+1)
	}
	if accept(100 - windowSize) {
		t.Fatalf("%d 落后 64，应当被拒", 100-windowSize)
	}
	// 只看不记：fresh 不改窗口。
	if !w.fresh(101) || !w.fresh(101) {
		t.Fatal("fresh 不应当改窗口")
	}
	if !accept(200) || !accept(150) || accept(136) {
		t.Fatal("窗口跳过一大段之后，旧位应当清零：150 收、136 落后 64 拒")
	}
}
