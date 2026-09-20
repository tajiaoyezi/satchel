package auth

import (
	"regexp"

	"golang.org/x/crypto/bcrypt"

	v1 "github.com/satchel/satchel/pkg/api/v1"
)

// 用户名与密码的规则（design 第 12 条）。
var usernameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{2,31}$`)

const (
	minPasswordLen = 8
	maxPasswordLen = 72 // bcrypt 只看前 72 字节，超过的不静默截断而是拒绝
)

// ValidateUsername 校验用户名：3 到 32 个字符，小写字母、数字、_ 与 -，以字母或数字开头。
func ValidateUsername(name string) error {
	if !usernameRe.MatchString(name) {
		return v1.Newf(v1.CodeBadRequest, "用户名 %q 不合规：3 到 32 个字符，只能用小写字母、数字、_ 与 -，且以字母或数字开头", name)
	}
	return nil
}

// ValidatePassword 校验密码：至少 8 个字符，最多 72 字节。
func ValidatePassword(pw string) error {
	if len([]rune(pw)) < minPasswordLen {
		return v1.Newf(v1.CodeBadRequest, "密码至少 %d 个字符", minPasswordLen)
	}
	if len(pw) > maxPasswordLen {
		return v1.Newf(v1.CodeBadRequest, "密码最多 %d 字节（bcrypt 的上限）", maxPasswordLen)
	}
	return nil
}

// dummyHash 给「账号不存在」那条路径比对用，让它与「密码不对」耗时接近。
var dummyHash = func() string {
	h, _ := bcrypt.GenerateFromPassword([]byte("satchel-dummy-password"), bcrypt.DefaultCost)
	return string(h)
}()

// HashPassword 用 bcrypt（默认 cost，与 mmwx 一致，M9 导入的哈希能直接用）。
func HashPassword(pw string) (string, error) {
	h, err := bcrypt.GenerateFromPassword([]byte(pw), bcrypt.DefaultCost)
	if err != nil {
		return "", v1.Wrap(v1.CodeInternal, "生成密码哈希失败", err)
	}
	return string(h), nil
}

// CheckPassword 比对密码与哈希。
func CheckPassword(hash, pw string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(pw)) == nil
}
