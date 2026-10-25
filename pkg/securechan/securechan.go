// Package securechan 是主控与节点之间的端到端加密（技术方案第 06 章，从 mmwx internal/securechan 搬来）：
// 主控用长期的 Ed25519 身份签握手，双方各出一把 X25519 临时密钥做 ECDH，经 HKDF-SHA256 派生两个方向各自的
// AES-256-GCM 密钥与基础 nonce；每帧是 [版本 0x01][序号 uint64 大端][密文与认证标签]，收方用 64 格滑动窗口防重放。
//
// 相对 mmwx 改了两处（m2-02 design 第 3 条）：签名覆盖「协议标签 + 节点临时公钥 + 主控临时公钥」，不只签主控那一把，
// 签名因此只对这一次握手有效；防重放窗口在解密成功之后才前移，伪造的高序号帧推不动窗口。
// 本包只依赖标准库与 golang.org/x/crypto，主控与 satchel-agent 共用（不复制第二份）。
package securechan

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"

	"golang.org/x/crypto/curve25519"
	"golang.org/x/crypto/hkdf"
)

const (
	// Label 是协议标签：写进握手签名的开头，也是 HKDF 的 info。换协议时换标签，旧签名与旧密钥就对不上。
	Label = "satchel-securechan-v1"
	// EnvelopeVersion 是加密信封的第一个字节。
	EnvelopeVersion = 0x01

	envelopeHeader = 1 + 8 // 版本 1 字节 + 序号 8 字节
	gcmTagSize     = 16
	nonceSize      = 12
	windowSize     = 64
	// KeySize 是 X25519 临时公钥的长度。
	KeySize = 32
)

// ParsePublicKey 解析标准 base64 的 Ed25519 公钥（安装命令与节点配置里的写法）。
func ParsePublicKey(b64 string) (ed25519.PublicKey, error) {
	data, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return nil, fmt.Errorf("主控公钥不是合法的 base64：%w", err)
	}
	if len(data) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("主控公钥长度应当是 %d 字节，实际 %d", ed25519.PublicKeySize, len(data))
	}
	return ed25519.PublicKey(data), nil
}

// EncodePublicKey 把 Ed25519 公钥写成标准 base64。
func EncodePublicKey(pub ed25519.PublicKey) string {
	return base64.StdEncoding.EncodeToString(pub)
}

// Fingerprint 是公钥原始 32 字节的 SHA-256，十六进制小写：安装脚本拿它核对命令里带的主控公钥。
func Fingerprint(pub ed25519.PublicKey) string {
	sum := sha256.Sum256(pub)
	return hex.EncodeToString(sum[:])
}

// GenerateEphemeral 生成一对 X25519 临时密钥。
func GenerateEphemeral() (priv, pub []byte, err error) {
	priv = make([]byte, KeySize)
	if _, err = rand.Read(priv); err != nil {
		return nil, nil, err
	}
	pub, err = curve25519.X25519(priv, curve25519.Basepoint)
	if err != nil {
		return nil, nil, err
	}
	return priv, pub, nil
}

// ComputeSharedSecret 做 X25519 ECDH；对方给的是低阶点（共享密钥全零）时返回错误。
func ComputeSharedSecret(myPriv, theirPub []byte) ([]byte, error) {
	return curve25519.X25519(myPriv, theirPub)
}

func transcript(agentEphPub, masterEphPub []byte) []byte {
	out := make([]byte, 0, len(Label)+len(agentEphPub)+len(masterEphPub))
	out = append(out, Label...)
	out = append(out, agentEphPub...)
	return append(out, masterEphPub...)
}

// SignHandshake 是主控对这一次握手的签名：签「协议标签 + 节点临时公钥 + 主控临时公钥」。
func SignHandshake(priv ed25519.PrivateKey, agentEphPub, masterEphPub []byte) []byte {
	return ed25519.Sign(priv, transcript(agentEphPub, masterEphPub))
}

// VerifyHandshake 是节点用配置里的主控公钥验这一次握手的签名。
func VerifyHandshake(pub ed25519.PublicKey, agentEphPub, masterEphPub, sig []byte) bool {
	if len(pub) != ed25519.PublicKeySize || len(agentEphPub) != KeySize || len(masterEphPub) != KeySize {
		return false
	}
	return ed25519.Verify(pub, transcript(agentEphPub, masterEphPub), sig)
}

// Session 是建立好的会话：两个方向各一把 AES-256-GCM，发送序号从 1 起，收方带防重放窗口。可以并发收发。
type Session struct {
	sendCipher cipher.AEAD
	recvCipher cipher.AEAD
	sendSeq    atomic.Uint64
	sendNonce  [nonceSize]byte
	recvNonce  [nonceSize]byte

	recvMu     sync.Mutex
	recvWindow replayWindow
}

// replayWindow 是滑动窗口：maxSeq 是收到过的最大序号，bitmap 的第 i 位表示 maxSeq-i 收过。
type replayWindow struct {
	maxSeq uint64
	bitmap uint64
}

// fresh 报告 seq 能不能收：不为 0、没收过、不比最大序号落后 windowSize 及以上。只看不改。
func (w *replayWindow) fresh(seq uint64) bool {
	if seq == 0 {
		return false
	}
	if seq > w.maxSeq {
		return true
	}
	diff := w.maxSeq - seq
	return diff < windowSize && w.bitmap&(uint64(1)<<diff) == 0
}

// mark 记下 seq 已收，必要时前移窗口。只在解密成功之后调用。
func (w *replayWindow) mark(seq uint64) {
	if seq > w.maxSeq {
		shift := seq - w.maxSeq
		if shift >= windowSize {
			w.bitmap = 0
		} else {
			w.bitmap <<= shift
		}
		w.maxSeq = seq
		w.bitmap |= 1
		return
	}
	w.bitmap |= uint64(1) << (w.maxSeq - seq)
}

// DeriveSession 由共享密钥派生会话。isMaster 决定哪个方向用哪把密钥：主控发用「主控到节点」，收用「节点到主控」。
func DeriveSession(sharedSecret, agentEphPub, masterEphPub []byte, isMaster bool) (*Session, error) {
	salt := make([]byte, 0, len(agentEphPub)+len(masterEphPub))
	salt = append(salt, agentEphPub...)
	salt = append(salt, masterEphPub...)
	hk := hkdf.New(sha256.New, sharedSecret, salt, []byte(Label))

	// HKDF 依次读出：主控到节点的密钥、节点到主控的密钥、主控到节点的基础 nonce、节点到主控的基础 nonce。
	m2aKey, a2mKey := make([]byte, 32), make([]byte, 32)
	m2aNonce, a2mNonce := make([]byte, nonceSize), make([]byte, nonceSize)
	for _, b := range [][]byte{m2aKey, a2mKey, m2aNonce, a2mNonce} {
		if _, err := io.ReadFull(hk, b); err != nil {
			return nil, fmt.Errorf("hkdf：%w", err)
		}
	}
	sendKey, recvKey, sendNonce, recvNonce := a2mKey, m2aKey, a2mNonce, m2aNonce
	if isMaster {
		sendKey, recvKey, sendNonce, recvNonce = m2aKey, a2mKey, m2aNonce, a2mNonce
	}
	sendCipher, err := newGCM(sendKey)
	if err != nil {
		return nil, err
	}
	recvCipher, err := newGCM(recvKey)
	if err != nil {
		return nil, err
	}
	s := &Session{sendCipher: sendCipher, recvCipher: recvCipher}
	copy(s.sendNonce[:], sendNonce)
	copy(s.recvNonce[:], recvNonce)
	return s, nil
}

func newGCM(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// Encrypt 生成一帧信封：[版本][序号][密文与标签]。
func (s *Session) Encrypt(plaintext []byte) ([]byte, error) {
	seq := s.sendSeq.Add(1)
	nonce := makeNonce(s.sendNonce, seq)
	out := make([]byte, envelopeHeader, envelopeHeader+len(plaintext)+gcmTagSize)
	out[0] = EnvelopeVersion
	binary.BigEndian.PutUint64(out[1:envelopeHeader], seq)
	return s.sendCipher.Seal(out, nonce[:], plaintext, nil), nil
}

// ErrReplay 是序号为 0、重复或落在窗口之外。
var ErrReplay = errors.New("securechan：序号为 0、已收过或落在防重放窗口之外")

// Decrypt 解开一帧信封。序号先只检查不记，解密成功之后才记进窗口；整个过程持锁，同一序号不会被并发收两次。
func (s *Session) Decrypt(envelope []byte) ([]byte, error) {
	if len(envelope) < envelopeHeader+gcmTagSize {
		return nil, errors.New("securechan：信封太短")
	}
	if envelope[0] != EnvelopeVersion {
		return nil, fmt.Errorf("securechan：不认识的信封版本 %d", envelope[0])
	}
	seq := binary.BigEndian.Uint64(envelope[1:envelopeHeader])
	s.recvMu.Lock()
	defer s.recvMu.Unlock()
	if !s.recvWindow.fresh(seq) {
		return nil, ErrReplay
	}
	nonce := makeNonce(s.recvNonce, seq)
	plaintext, err := s.recvCipher.Open(nil, nonce[:], envelope[envelopeHeader:], nil)
	if err != nil {
		return nil, fmt.Errorf("securechan：解密失败：%w", err)
	}
	s.recvWindow.mark(seq)
	return plaintext, nil
}

// makeNonce 是基础 nonce 与序号（大端放在后 8 字节）的异或。
func makeNonce(base [nonceSize]byte, seq uint64) [nonceSize]byte {
	nonce := base
	var seqBytes [nonceSize]byte
	binary.BigEndian.PutUint64(seqBytes[4:], seq)
	for i := range nonce {
		nonce[i] ^= seqBytes[i]
	}
	return nonce
}
