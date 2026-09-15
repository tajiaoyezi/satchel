// Package release 是契约层里发布物的验签：Satchel 的发布公钥清单与 Ed25519 分离签名的校验。
// 三个二进制（satchel、satchel-agent、satchel-speedtester）共用一把发布密钥；公钥编进二进制，
// satchel-agent 到 M2 直接引用本包验自己的升级包，公钥只维护这一处（技术方案第 09 章）。
//
// 轮换分两期：先发一版清单里同时有新旧两把公钥、新包用新钥签，下一版再去掉旧公钥；
// 绝不能直接切成只带新公钥，否则线上旧节点永远验不过新包。
package release

import (
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"strings"
)

// publicKeysCSV 是发布公钥清单的源文本：base64 的 32 字节 Ed25519 公钥，逗号分隔，新钥在前。
// 正式公钥写在这里、随源码审计；它是字符串只为了让测试与本地演练能用 ldflags -X 换成测试公钥
// （go build -ldflags "-X github.com/satchel/satchel/pkg/release.publicKeysCSV=<base64>"），发布线不用这个口子。
var publicKeysCSV = "" +
	// 2026-09-14 生成的正式发布公钥（第一把；轮换时在前面加新钥，下一版再去掉这一把）。
	"Gp8PtBixDGF/pHfQM4LZ6mSJuE8cRJ2vJiTUYxrC7rI="

// PublicKeys 是解析后的发布公钥清单。验签对清单里任意一把通过即通过；测试可以整个替换它。
var PublicKeys = splitKeys(publicKeysCSV)

func splitKeys(csv string) []string {
	var keys []string
	for _, k := range strings.Split(csv, ",") {
		if k = strings.TrimSpace(k); k != "" {
			keys = append(keys, k)
		}
	}
	return keys
}

// SignatureSize 是分离签名文件的固定长度：Ed25519 原始签名 64 字节，不带任何封装。
const SignatureSize = ed25519.SignatureSize

// ErrBadSignature 表示签名与清单里的每一把公钥都对不上。
var ErrBadSignature = errors.New("签名与发布公钥不匹配")

// Verify 用清单里的公钥逐把验 data 的分离签名 sig；都对不上返回 ErrBadSignature。
func Verify(data, sig []byte) error {
	if len(sig) != SignatureSize {
		return fmt.Errorf("签名长度应当是 %d 字节，得到 %d", SignatureSize, len(sig))
	}
	if len(PublicKeys) == 0 {
		return errors.New("发布公钥清单为空，无法验签")
	}
	for _, encoded := range PublicKeys {
		key, err := decodePublicKey(encoded)
		if err != nil {
			return err
		}
		if ed25519.Verify(key, data, sig) {
			return nil
		}
	}
	return ErrBadSignature
}

// VerifyFile 读 path 与 sigPath 后验签。
func VerifyFile(path, sigPath string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("读取待验文件失败：%w", err)
	}
	sig, err := os.ReadFile(sigPath)
	if err != nil {
		return fmt.Errorf("读取签名文件失败：%w", err)
	}
	return Verify(data, sig)
}

// decodePublicKey 把 base64 的公钥解成 ed25519.PublicKey，长度不对就报错——清单写错是编程错误，不该静默跳过。
func decodePublicKey(encoded string) (ed25519.PublicKey, error) {
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("发布公钥不是合法的 base64：%w", err)
	}
	if len(raw) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("发布公钥应当是 %d 字节，得到 %d", ed25519.PublicKeySize, len(raw))
	}
	return ed25519.PublicKey(raw), nil
}
