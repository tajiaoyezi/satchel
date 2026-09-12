package v1

import (
	"bytes"
	"encoding/json"
)

// Redacted 是打码字段序列化时输出的标记。
const Redacted = "***"

// Secret 是打码的文本字段（密码哈希、令牌、凭据）。序列化默认输出 Redacted，
// 忘了打码也不会把原文写出去；要原文得显式调 Reveal。空值仍输出空字符串，调用方能看出「没设」。
// 反序列化原样收下：apply 收到 Redacted 时表示「保持不变」，由 apply 层处理。
type Secret string

// Reveal 返回原文。只有持「密钥读取」权限的输出路径才该调它。
func (s Secret) Reveal() string { return string(s) }

// IsRedacted 报告这个值是不是打码标记（apply 收到它时表示保持不变）。
func (s Secret) IsRedacted() bool { return string(s) == Redacted }

func (s Secret) MarshalJSON() ([]byte, error) {
	if s == "" {
		return []byte(`""`), nil
	}
	return json.Marshal(Redacted)
}

func (s *Secret) UnmarshalJSON(data []byte) error {
	var v string
	if err := json.Unmarshal(data, &v); err != nil {
		return err
	}
	*s = Secret(v)
	return nil
}

// SecretJSON 是打码的 JSON 字段（凭据 JSON、含私钥的入站出站参数）。序列化默认输出 Redacted 字符串，
// 空值输出 null；要原文得显式调 Reveal。
type SecretJSON json.RawMessage

// Reveal 返回原始 JSON。
func (s SecretJSON) Reveal() json.RawMessage { return json.RawMessage(s) }

// IsRedacted 报告这个值是不是打码标记。
func (s SecretJSON) IsRedacted() bool {
	return bytes.Equal(bytes.TrimSpace(s), []byte(`"`+Redacted+`"`))
}

func (s SecretJSON) MarshalJSON() ([]byte, error) {
	if len(bytes.TrimSpace(s)) == 0 || bytes.Equal(bytes.TrimSpace(s), []byte("null")) {
		return []byte("null"), nil
	}
	return json.Marshal(Redacted)
}

func (s *SecretJSON) UnmarshalJSON(data []byte) error {
	if !json.Valid(data) {
		return &json.SyntaxError{}
	}
	*s = append((*s)[:0], data...)
	return nil
}
