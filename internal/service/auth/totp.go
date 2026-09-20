package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"

	"github.com/pquerna/otp"
	"github.com/pquerna/otp/totp"

	"github.com/satchel/satchel/internal/core/users"
	v1 "github.com/satchel/satchel/pkg/api/v1"
)

// TOTPKey 是新生成的两步验证密钥。
type TOTPKey struct {
	Secret string `json:"secret"`
	URL    string `json:"otpauth_url"`
}

// newTOTPKey 生成一把密钥（照 mmwx：pquerna/otp 默认参数，30 秒、6 位、SHA1）。
func newTOTPKey(username string) (TOTPKey, error) {
	key, err := totp.Generate(totp.GenerateOpts{Issuer: Issuer, AccountName: username})
	if err != nil {
		return TOTPKey{}, v1.Wrap(v1.CodeInternal, "生成两步验证密钥失败", err)
	}
	return TOTPKey{Secret: key.Secret(), URL: key.URL()}, nil
}

// totpOpts 是 pquerna/otp 的默认参数（30 秒、6 位、SHA1、允许前后各一个周期），显式写出来是为了能按 s.now() 校验（测试里可以拨时钟）。
var totpOpts = totp.ValidateOpts{Period: 30, Skew: 1, Digits: otp.DigitsSix, Algorithm: otp.AlgorithmSHA1}

// checkTOTP 校验一个 TOTP 码并做防重放：同一账号的同一个码在宽限期内只认一次（登录与当场验证合计）。
func (s *Service) checkTOTP(username, secret, code string) bool {
	code = strings.TrimSpace(code)
	if ok, err := totp.ValidateCustom(code, secret, s.now(), totpOpts); err != nil || !ok {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	for k, exp := range s.usedTOTP {
		if exp.Before(now) {
			delete(s.usedTOTP, k)
		}
	}
	key := username + "\x00" + code
	if _, used := s.usedTOTP[key]; used {
		return false
	}
	s.usedTOTP[key] = now.Add(totpReplayGrace)
	return true
}

// 恢复码：8 枚，各 8 个小写十六进制字符；库里存 SHA-256 十六进制。
const recoveryCodeCount = 8

func generateRecoveryCodes() (plain, hashes []string, err error) {
	for i := 0; i < recoveryCodeCount; i++ {
		buf := make([]byte, 4)
		if _, err := rand.Read(buf); err != nil {
			return nil, nil, v1.Wrap(v1.CodeInternal, "生成恢复码失败", err)
		}
		code := hex.EncodeToString(buf)
		plain = append(plain, code)
		hashes = append(hashes, hashRecoveryCode(code))
	}
	return plain, hashes, nil
}

func hashRecoveryCode(code string) string {
	sum := sha256.Sum256([]byte(strings.ToLower(strings.TrimSpace(code))))
	return hex.EncodeToString(sum[:])
}

// looksLikeRecoveryCode 区分第二因素的两种形状：TOTP 是 6 位数字，恢复码是 8 个十六进制字符。
func looksLikeRecoveryCode(code string) bool {
	code = strings.TrimSpace(code)
	if len(code) != 8 {
		return false
	}
	_, err := hex.DecodeString(code)
	return err == nil
}

// consumeRecoveryCode 校验并作废一枚恢复码：在同一个数据库事务里「读清单、去掉这一枚、带前置条件写回」，
// 前置条件不满足（并发的另一个请求先用掉了）就当作没匹配。返回是否成功。
func (s *Service) consumeRecoveryCode(ctx context.Context, a *users.Account, code string) (bool, error) {
	target := hashRecoveryCode(code)
	idx := -1
	for i, h := range a.RecoveryCodes {
		if h == target {
			idx = i
			break
		}
	}
	if idx < 0 {
		return false, nil
	}
	remaining := append(append([]string{}, a.RecoveryCodes[:idx]...), a.RecoveryCodes[idx+1:]...)
	err := s.users.SetRecoveryCodes(ctx, a.ID, remaining, a.RecoveryCodes)
	if err != nil {
		var e *v1.Error
		if errors.As(err, &e) && e.Code == v1.CodeConflict {
			return false, nil // 清单已经变了：这一枚被别人先用掉了，或清单被重新生成
		}
		return false, err
	}
	a.RecoveryCodes = remaining
	return true, nil
}

// checkSecondFactor 用 TOTP 或恢复码验第二因素；恢复码通过时已作废。
func (s *Service) checkSecondFactor(ctx context.Context, a *users.Account, code string) (bool, error) {
	code = strings.TrimSpace(code)
	if code == "" {
		return false, nil
	}
	if looksLikeRecoveryCode(code) {
		return s.consumeRecoveryCode(ctx, a, code)
	}
	return s.checkTOTP(a.Username, a.TOTPSecret, code), nil
}

// recoveryLow 报告剩余恢复码是否不足两枚。
func recoveryLow(a *users.Account) bool {
	return a.TOTPEnabled && len(a.RecoveryCodes) < 2
}
