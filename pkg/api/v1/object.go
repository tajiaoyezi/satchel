package v1

import (
	"bytes"
	"encoding/json"
	"regexp"
	"sort"
	"time"
)

// APIVersion 是本包所有对象的 apiVersion 值（第 07 章公共 schema）。
const APIVersion = "satchel/v1"

// Kind 是资源种类名，PascalCase，例如 Task、AutomationRule。
type Kind string

// Class 是 kind 的操作类别，对应第 07 章的五类操作表。
type Class string

const (
	// ClassConfig 配置类：spec + plan/apply，带 resourceVersion。
	ClassConfig Class = "config"
	// ClassAction 动作类：立即执行、进审计、用反向动作恢复。
	ClassAction Class = "action"
	// ClassMasterSettings 主控设置类：直接写、带 resourceVersion、不走 plan。
	ClassMasterSettings Class = "master_settings"
	// ClassSystem 系统只写：只由心跳、对账、内置任务写。
	ClassSystem Class = "system"
	// ClassReadOnly 只读展示：没有写接口。
	ClassReadOnly Class = "readonly"
)

// Metadata 是每个对象都有的元数据，字段名就是 JSON 输出的字段名。
type Metadata struct {
	// ID 是整数主键，只有有整数主键的 kind 才有。
	ID int64 `json:"id,omitempty"`
	// Name 是对象的自然键。
	Name string `json:"name"`
	// ResourceVersion 从 1 起，只随 spec 写入递增；系统只写的 kind 没有版本，为 0。
	ResourceVersion int64     `json:"resourceVersion"`
	CreatedAt       time.Time `json:"createdAt"`
	UpdatedAt       time.Time `json:"updatedAt"`
	// DeletedAt 非空表示已软删除。
	DeletedAt *time.Time `json:"deletedAt,omitempty"`
}

// Object 是资源信封：序列化后恰好五个顶层键。
// S 与 T 是该 kind 生成的 Spec / Status 结构体；尚未按 kind 解析时两者都用 json.RawMessage（见 RawObject）。
type Object[S, T any] struct {
	APIVersion string   `json:"apiVersion"`
	Kind       Kind     `json:"kind"`
	Metadata   Metadata `json:"metadata"`
	Spec       S        `json:"spec"`
	Status     T        `json:"status"`
}

// RawObject 是 spec 与 status 尚未按 kind 解析的信封。
type RawObject = Object[json.RawMessage, json.RawMessage]

var envelopeKeys = map[string]bool{"apiVersion": true, "kind": true, "metadata": true, "spec": true, "status": true}

// requiredKeys 是反序列化时必须出现的顶层键；status 可省略，apply 请求本来就不带它。
var requiredKeys = []string{"apiVersion", "kind", "metadata", "spec"}

// Decode 严格解码一个对象：apiVersion 必须是 APIVersion，顶层不允许五个键之外的字段，
// apiVersion、kind、metadata、spec 必须有，metadata 里不认识的键被拒。
// spec 内部的字段校验不在这里做，见 DecodeSpec。
func Decode[S, T any](data []byte) (*Object[S, T], error) {
	top, err := decodeObject(data, "对象")
	if err != nil {
		return nil, err
	}
	keys := make([]string, 0, len(top))
	for k := range top {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if !envelopeKeys[k] {
			return nil, Newf(CodeBadRequest, "对象顶层不允许有字段 %s，只认 apiVersion、kind、metadata、spec、status", k)
		}
	}
	// apiVersion 先于其它必填键：别的版本的对象形状可能不同，先把版本问题报出来。
	rawVersion, ok := top["apiVersion"]
	if !ok {
		return nil, New(CodeBadRequest, "对象缺少 apiVersion")
	}
	var version string
	if err := json.Unmarshal(rawVersion, &version); err != nil {
		return nil, Wrap(CodeBadRequest, "apiVersion 不是字符串", err)
	}
	if version != APIVersion {
		return nil, Newf(CodeUnsupportedAPIVersion, "不支持的 apiVersion %q，本版本只认 %s", version, APIVersion)
	}
	for _, k := range requiredKeys {
		if _, ok := top[k]; !ok {
			return nil, Newf(CodeBadRequest, "对象缺少 %s", k)
		}
	}
	// metadata、spec 与给了的 status 都必须是 JSON 对象：null 会被 encoding/json 静默解成零值。
	for _, k := range []string{"metadata", "spec", "status"} {
		raw, ok := top[k]
		if !ok {
			continue
		}
		if trimmed := bytes.TrimSpace(raw); len(trimmed) == 0 || trimmed[0] != '{' {
			return nil, Newf(CodeBadRequest, "%s 必须是 JSON 对象", k)
		}
	}
	var obj Object[S, T]
	if err := decodeMetadata(top["metadata"], &obj.Metadata); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, &obj); err != nil {
		return nil, Wrap(CodeBadRequest, "对象的字段类型不对", err)
	}
	return &obj, nil
}

// decodeObject 把一段 JSON 解成键到原文的映射；不是 JSON 对象（null、数组、标量、坏 JSON）时报 bad_request。
// what 是它在文案里的叫法（「对象」「spec」），拉丁字母的叫法后面自动补空格。
func decodeObject(data []byte, what string) (map[string]json.RawMessage, error) {
	if last := what[len(what)-1]; last < 0x80 && last != ' ' {
		what += " "
	}
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return nil, Newf(CodeBadRequest, "%s必须是 JSON 对象", what)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &m); err != nil {
		return nil, Wrap(CodeBadRequest, what+"不是合法的 JSON 对象", err)
	}
	return m, nil
}

var unknownFieldRE = regexp.MustCompile(`unknown field "([^"]*)"`)

// decodeMetadata 用拒绝未知字段的解码器解 metadata，把 encoding/json 的错误改写成点名字段的 bad_request。
func decodeMetadata(raw json.RawMessage, into *Metadata) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(into); err != nil {
		if m := unknownFieldRE.FindStringSubmatch(err.Error()); m != nil {
			return Wrap(CodeBadRequest, "metadata 里没有字段 "+m[1], err)
		}
		return Wrap(CodeBadRequest, "metadata 的字段类型不对", err)
	}
	return nil
}
