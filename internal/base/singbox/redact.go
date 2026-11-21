package singbox

import (
	"bytes"
	"encoding/json"
	"regexp"
	"strings"

	v1 "github.com/satchel/satchel/pkg/api/v1"
)

// sensitiveKeys 是在未持 secrets scope 时需要脱敏替换的常见配置与凭据键名（小写匹配）。
var sensitiveKeys = map[string]bool{
	"private_key":          true,
	"key_pem":              true,
	"password":             true,
	"uuid":                 true,
	"short_id":             true,
	"short_ids":            true,
	"pre_shared_key":       true,
	"psk":                  true,
	"secret":               true,
	"secret_key":           true,
	"api_key":              true,
	"access_key":           true,
	"client_secret":        true,
	"passphrase":           true,
	"obfs_password":        true,
	"token":                true,
	"agent_token":          true,
	"pull_token":           true,
	"tgbot_token":          true,
	"telegram_bot_token":   true,
	"turnstile_secret_key": true,
	"credentials":          true,
	"auth":                 true,
	"auth_str":             true,
	"userkey":              true,
	"decryption":           true,
}

// IsSensitiveKey 判断字段键名是否属于敏感键。
func IsSensitiveKey(key string) bool {
	k := strings.ToLower(strings.TrimSpace(key))
	if sensitiveKeys[k] {
		return true
	}
	for _, suffix := range []string{"_password", "_secret", "_private_key", "_token", "_psk"} {
		if strings.HasSuffix(k, suffix) {
			return true
		}
	}
	return false
}

// RedactValue 递归复制并打码 JSON 解码值中的敏感键（非空字符串或字符串数组替换为 "***"）。
func RedactValue(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, val := range x {
			if IsSensitiveKey(k) {
				out[k] = redactLeaf(val)
				continue
			}
			out[k] = RedactValue(val)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, item := range x {
			out[i] = RedactValue(item)
		}
		return out
	default:
		return v
	}
}

func redactLeaf(v any) any {
	switch x := v.(type) {
	case nil:
		return nil
	case string:
		if x == "" {
			return ""
		}
		return v1.Redacted
	case []any:
		out := make([]any, len(x))
		for i, item := range x {
			if s, ok := item.(string); ok {
				if s == "" {
					out[i] = ""
				} else {
					out[i] = v1.Redacted
				}
			} else {
				out[i] = RedactValue(item)
			}
		}
		return out
	case map[string]any:
		if len(x) == 0 {
			return x
		}
		return v1.Redacted
	default:
		return v1.Redacted
	}
}

// RedactJSON 将 JSON 字节中的敏感键脱敏；若 pretty 为真则输出 2 空格缩进格式，否则输出紧凑 JSON。
// 输入不是合法 JSON 时原样经文本脱敏规则返回。
func RedactJSON(raw []byte, pretty bool) []byte {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return []byte(RedactText(string(raw)))
	}
	redacted := RedactValue(v)
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if pretty {
		enc.SetIndent("", "  ")
	}
	if err := enc.Encode(redacted); err != nil {
		return []byte(RedactText(string(raw)))
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte{'\n'})
}

var (
	bearerLineRe = regexp.MustCompile(`(?i)(authorization["']?\s*[:=]\s*["']?bearer\s+)([^\s"';]+)`)
	kvSecretRe   = regexp.MustCompile(`(?i)("?(?:private_key|password|uuid|short_id|pre_shared_key|psk|secret|token|agent_token|pull_token|auth_str|userkey|decryption)"?\s*[:=]\s*"?)([^"\s,;}\]]+)`)
)

// RedactText 对纯文本或非 JSON 配置文件内容做保守敏感值脱敏（用于无 secrets scope 读 nginx 或 JSONC 文件）。
func RedactText(content string) string {
	trimmed := strings.TrimSpace(content)
	if len(trimmed) > 0 && (trimmed[0] == '{' || trimmed[0] == '[') && json.Valid([]byte(trimmed)) {
		return string(RedactJSON([]byte(trimmed), true))
	}
	s := bearerLineRe.ReplaceAllString(content, "${1}"+v1.Redacted)
	s = kvSecretRe.ReplaceAllString(s, "${1}"+v1.Redacted)
	return s
}
