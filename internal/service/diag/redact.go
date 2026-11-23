package diag

import (
	"encoding/json"
	"regexp"
	"strings"

	v1 "github.com/satchel/satchel/pkg/api/v1"
)

var (
	rePEMPrivateKey = regexp.MustCompile(`(?s)-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----.*?-----END [A-Z0-9 ]*PRIVATE KEY-----`)
	reSatchelToken  = regexp.MustCompile(`\bsat_[A-Za-z0-9_-]{8,}\b`)
	reBearerToken   = regexp.MustCompile(`(?i)\b(Bearer\s+)[A-Za-z0-9._~+/=-]{8,}`)
	reKVSecret      = regexp.MustCompile(`(?i)((?:token|agent_token|pull_token|password|passwd|secret|private_key|privateKey|pre_shared_key|preSharedKey|psk|api_key|apiKey|authorization|uuid|short_id|shortId|client_secret|access_token|refresh_token)["']?\s*[:=]\s*["']?)([^\s"',}\]]+)`)
)

// RedactSensitiveText 按已知密钥模式打码文本中的 PEM 私钥、sat_* 令牌、Bearer 凭据与常见键值对敏感值。
func RedactSensitiveText(s string) string {
	if s == "" {
		return ""
	}
	out := rePEMPrivateKey.ReplaceAllString(s, v1.Redacted)
	out = reSatchelToken.ReplaceAllString(out, v1.Redacted)
	out = reBearerToken.ReplaceAllString(out, "${1}"+v1.Redacted)
	out = reKVSecret.ReplaceAllString(out, "${1}"+v1.Redacted)
	return out
}

func isSensitiveKey(k string) bool {
	lk := strings.ToLower(strings.TrimSpace(k))
	switch lk {
	case "private_key", "privatekey", "password", "passwd", "token", "agent_token", "pull_token",
		"uuid", "psk", "pre_shared_key", "presharedkey", "short_id", "short_ids", "shortid",
		"secret", "client_secret", "access_token", "refresh_token", "authorization", "auth_token", "api_key", "apikey":
		return true
	}
	return strings.Contains(lk, "private_key") || strings.Contains(lk, "password") || strings.Contains(lk, "secret")
}

// redactAny 对任意可 JSON 序列化的值递归脱敏：敏感键置为 ***，长文本字符串套用已知密钥模式打码。
func redactAny(v any) any {
	if v == nil {
		return nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return v
	}
	var generic any
	if err := json.Unmarshal(b, &generic); err != nil {
		return v
	}
	return redactValue(generic)
}

func redactValue(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, val := range x {
			if isSensitiveKey(k) {
				switch val.(type) {
				case []any:
					arr := make([]any, len(val.([]any)))
					for i := range arr {
						arr[i] = v1.Redacted
					}
					out[k] = arr
				case string:
					if val.(string) == "" {
						out[k] = ""
					} else {
						out[k] = v1.Redacted
					}
				default:
					out[k] = v1.Redacted
				}
				continue
			}
			out[k] = redactValue(val)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, elem := range x {
			out[i] = redactValue(elem)
		}
		return out
	case string:
		return RedactSensitiveText(x)
	default:
		return v
	}
}
