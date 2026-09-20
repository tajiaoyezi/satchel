package settings

import (
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"strconv"

	"github.com/satchel/satchel/internal/base/schema"
	v1 "github.com/satchel/satchel/pkg/api/v1"
)

// 键值表的 value 是文本，按目录里的类型读写（storage-schema「Settings key catalog」的编码约定）：
// 布尔读时接受 1 / 0 / true / false / 空，写时统一 true / false；整数规范十进制；json 原文。
// 字段值在 Go 里的类型固定为：bool → bool、int → int64、text → string、json → json.RawMessage。

var decimalRe = regexp.MustCompile(`^-?(0|[1-9][0-9]*)$`)

// Decode 把文本按类型解成 Go 值。解不出报 bad_request，reason 点名值。
func Decode(t schema.Type, raw string) (any, error) {
	switch t {
	case schema.TypeBool:
		switch raw {
		case "", "0", "false":
			return false, nil
		case "1", "true":
			return true, nil
		}
		return nil, v1.Newf(v1.CodeBadRequest, "值 %q 不是布尔值（接受 true / false / 1 / 0）", raw)
	case schema.TypeInt:
		if !decimalRe.MatchString(raw) {
			return nil, v1.Newf(v1.CodeBadRequest, "值 %q 不是规范的十进制整数", raw)
		}
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			return nil, v1.Newf(v1.CodeBadRequest, "值 %q 超出整数范围", raw)
		}
		return n, nil
	case schema.TypeText:
		return raw, nil
	case schema.TypeJSON:
		if !json.Valid([]byte(raw)) {
			return nil, v1.Newf(v1.CodeBadRequest, "值 %q 不是合法的 JSON", raw)
		}
		return json.RawMessage(raw), nil
	}
	return nil, v1.Newf(v1.CodeInternal, "设置字段的类型 %s 不认识", t)
}

// Encode 把 Go 值按类型编成键值表里的文本。
func Encode(t schema.Type, v any) (string, error) {
	switch t {
	case schema.TypeBool:
		b, ok := v.(bool)
		if !ok {
			return "", v1.Newf(v1.CodeInternal, "布尔字段的值是 %T", v)
		}
		return strconv.FormatBool(b), nil
	case schema.TypeInt:
		n, ok := v.(int64)
		if !ok {
			return "", v1.Newf(v1.CodeInternal, "整数字段的值是 %T", v)
		}
		return strconv.FormatInt(n, 10), nil
	case schema.TypeText:
		s, ok := v.(string)
		if !ok {
			return "", v1.Newf(v1.CodeInternal, "文本字段的值是 %T", v)
		}
		return s, nil
	case schema.TypeJSON:
		raw, ok := v.(json.RawMessage)
		if !ok {
			return "", v1.Newf(v1.CodeInternal, "JSON 字段的值是 %T", v)
		}
		return string(raw), nil
	}
	return "", v1.Newf(v1.CodeInternal, "设置字段的类型 %s 不认识", t)
}

// Normalize 把写请求里的值归一成字段类型的 Go 值：值是字符串时按编码规则解析（文本字段就是它本身），
// 值是 JSON 原生类型时按目录类型收（布尔、整数、字符串、任意 JSON 值）。对不上报 bad_request。
func Normalize(t schema.Type, v any) (any, error) {
	if s, ok := v.(string); ok {
		return Decode(t, s)
	}
	switch t {
	case schema.TypeBool:
		if b, ok := v.(bool); ok {
			return b, nil
		}
	case schema.TypeInt:
		switch n := v.(type) {
		case int64:
			return n, nil
		case int:
			return int64(n), nil
		case float64:
			if n == math.Trunc(n) && math.Abs(n) < 1<<53 {
				return int64(n), nil
			}
		case json.Number:
			if i, err := n.Int64(); err == nil {
				return i, nil
			}
		}
	case schema.TypeText:
		// 只收字符串，上面已经处理；到这里就是别的类型。
	case schema.TypeJSON:
		if raw, ok := v.(json.RawMessage); ok {
			if !json.Valid(raw) {
				return nil, v1.New(v1.CodeBadRequest, "值不是合法的 JSON")
			}
			return raw, nil
		}
		raw, err := json.Marshal(v)
		if err != nil {
			return nil, v1.Wrap(v1.CodeBadRequest, "值不能编成 JSON", err)
		}
		return json.RawMessage(raw), nil
	}
	return nil, v1.Newf(v1.CodeBadRequest, "值 %s 不是%s", describe(v), typeLabel(t))
}

func describe(v any) string {
	raw, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("%v", v)
	}
	return string(raw)
}

func typeLabel(t schema.Type) string {
	switch t {
	case schema.TypeBool:
		return "布尔值"
	case schema.TypeInt:
		return "整数"
	case schema.TypeText:
		return "字符串"
	case schema.TypeJSON:
		return "JSON"
	}
	return t.String()
}
