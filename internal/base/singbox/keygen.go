package singbox

import (
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strings"

	v1 "github.com/satchel/satchel/pkg/api/v1"
)

// KeygenResult 是 inbound keygen 返回的随机密钥/凭据集合。
type KeygenResult struct {
	Type                string `json:"type"`
	UUID                string `json:"uuid,omitempty"`
	RealityPrivateKey   string `json:"reality_private_key,omitempty"`
	RealityPublicKey    string `json:"reality_public_key,omitempty"`
	ShortID             string `json:"short_id,omitempty"`
	Password            string `json:"password,omitempty"`
	SS2022Cipher        string `json:"ss2022_cipher,omitempty"`
	SS2022PSK           string `json:"ss2022_psk,omitempty"`
	WireGuardPrivateKey string `json:"wireguard_private_key,omitempty"`
	WireGuardPublicKey  string `json:"wireguard_public_key,omitempty"`
}

// GenerateUUID 生成标准 RFC 4122 UUIDv4 字符串。
func GenerateUUID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", v1.Wrap(v1.CodeInternal, "生成 UUID 随机数失败", err)
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}

// GenerateRealityKeypair 使用标准库 X25519 生成与 sing-box / xray 兼容的 Reality 密钥对（Base64 RawURL）。
func GenerateRealityKeypair() (privateKey, publicKey string, err error) {
	var seed [32]byte
	if _, err := rand.Read(seed[:]); err != nil {
		return "", "", v1.Wrap(v1.CodeInternal, "生成 X25519 随机私钥失败", err)
	}
	// RFC 7748 clamping，与 sing-box generate reality-keypair 保持一致。
	seed[0] &= 248
	seed[31] &= 127
	seed[31] |= 64
	priv, err := ecdh.X25519().NewPrivateKey(seed[:])
	if err != nil {
		return "", "", v1.Wrap(v1.CodeInternal, "构造 X25519 私钥失败", err)
	}
	return base64.RawURLEncoding.EncodeToString(priv.Bytes()),
		base64.RawURLEncoding.EncodeToString(priv.PublicKey().Bytes()), nil
}

// DeriveRealityPublicKey 由 Reality X25519 私钥实时派生 Base64 RawURL 公钥（兼容 RawURL 与 StdEncoding）。
func DeriveRealityPublicKey(privateKey string) (string, error) {
	trimmed := strings.TrimSpace(privateKey)
	if trimmed == "" {
		return "", v1.New(v1.CodeBadRequest, "Reality 私钥为空")
	}
	var raw []byte
	var err error
	for _, enc := range []*base64.Encoding{base64.RawURLEncoding, base64.URLEncoding, base64.StdEncoding, base64.RawStdEncoding} {
		raw, err = enc.DecodeString(trimmed)
		if err == nil && len(raw) == 32 {
			break
		}
	}
	if err != nil || len(raw) != 32 {
		return "", v1.New(v1.CodeBadRequest, "Reality 私钥不是合法的 32 字节 Base64 密钥")
	}
	priv, err := ecdh.X25519().NewPrivateKey(raw)
	if err != nil {
		return "", v1.Wrap(v1.CodeBadRequest, "解析 Reality 私钥失败", err)
	}
	return base64.RawURLEncoding.EncodeToString(priv.PublicKey().Bytes()), nil
}

// GenerateWireGuardKeypair 生成 WireGuard 标准 Base64 X25519 密钥对。
func GenerateWireGuardKeypair() (privateKey, publicKey string, err error) {
	priv, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return "", "", v1.Wrap(v1.CodeInternal, "生成 WireGuard 密钥对失败", err)
	}
	return base64.StdEncoding.EncodeToString(priv.Bytes()),
		base64.StdEncoding.EncodeToString(priv.PublicKey().Bytes()), nil
}

// GenerateShortID 生成指定字节长度的小写十六进制 Reality short_id（默认 4 字节 = 8 位 hex）。
func GenerateShortID(byteLen int) (string, error) {
	if byteLen <= 0 || byteLen > 8 {
		byteLen = 4
	}
	b := make([]byte, byteLen)
	if _, err := rand.Read(b); err != nil {
		return "", v1.Wrap(v1.CodeInternal, "生成 short_id 失败", err)
	}
	return hex.EncodeToString(b), nil
}

// GeneratePassword 生成指定字节数的 Base64 RawURL 随机密码（默认 16 字节）。
func GeneratePassword(byteLen int) (string, error) {
	if byteLen <= 0 {
		byteLen = 16
	}
	b := make([]byte, byteLen)
	if _, err := rand.Read(b); err != nil {
		return "", v1.Wrap(v1.CodeInternal, "生成随机密码失败", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// GenerateSS2022PSK 按 Shadowsocks 2022 算法生成对应字节长度的标准 Base64 PSK。
func GenerateSS2022PSK(cipher string) (string, error) {
	c := strings.TrimSpace(cipher)
	if c == "" {
		c = "2022-blake3-aes-128-gcm"
	}
	n, ok := SS2022KeyBytes[c]
	if !ok {
		return "", v1.Newf(v1.CodeBadRequest, "不支持的 SS2022 算法 %q（支持 2022-blake3-aes-128-gcm、2022-blake3-aes-256-gcm、2022-blake3-chacha20-poly1305）", cipher)
	}
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", v1.Wrap(v1.CodeInternal, "生成 SS2022 PSK 失败", err)
	}
	return base64.StdEncoding.EncodeToString(b), nil
}

// Keygen 按类型生成随机密钥材料。
func Keygen(typ, cipher string) (*KeygenResult, error) {
	t := strings.ToLower(strings.TrimSpace(typ))
	if t == "" {
		t = "all"
	}
	c := strings.TrimSpace(cipher)
	if c == "" {
		c = "2022-blake3-aes-128-gcm"
	}
	res := &KeygenResult{Type: t}
	wantAll := t == "all"
	switch t {
	case "all", "uuid", "x25519", "short-id", "password", "ss2022", "wireguard":
	default:
		return nil, v1.Newf(v1.CodeBadRequest, "不支持的密钥类型 %q（支持 uuid、x25519、short-id、password、ss2022、wireguard、all）", typ)
	}
	if wantAll || t == "uuid" {
		u, err := GenerateUUID()
		if err != nil {
			return nil, err
		}
		res.UUID = u
	}
	if wantAll || t == "x25519" {
		priv, pub, err := GenerateRealityKeypair()
		if err != nil {
			return nil, err
		}
		res.RealityPrivateKey = priv
		res.RealityPublicKey = pub
	}
	if wantAll || t == "short-id" {
		sid, err := GenerateShortID(4)
		if err != nil {
			return nil, err
		}
		res.ShortID = sid
	}
	if wantAll || t == "password" {
		pw, err := GeneratePassword(16)
		if err != nil {
			return nil, err
		}
		res.Password = pw
	}
	if wantAll || t == "ss2022" {
		psk, err := GenerateSS2022PSK(c)
		if err != nil {
			return nil, err
		}
		res.SS2022Cipher = c
		res.SS2022PSK = psk
	}
	if wantAll || t == "wireguard" {
		priv, pub, err := GenerateWireGuardKeypair()
		if err != nil {
			return nil, err
		}
		res.WireGuardPrivateKey = priv
		res.WireGuardPublicKey = pub
	}
	return res, nil
}
