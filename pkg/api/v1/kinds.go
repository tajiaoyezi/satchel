package v1

import (
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"strings"
)

// KindInfo 是 kind 清单里的一项，由表注册表生成（zz_generated_kinds.go），不手写第二份。
type KindInfo struct {
	Name  Kind
	Class Class
	// SpecFields 是 spec 里的字段名，与列名一致。
	SpecFields []string
	// StatusFields 是 status 里的字段名：status、动作专属、人类专属、主控自身类、只读五档都在这里。
	StatusFields []string
	// MaskedFields 是输出时要按第 05 章打码的字段名（密钥、令牌之类）。
	MaskedFields []string
	// ImmutableFields 是创建后不能改的 spec 字段（自然主键，如 User 的 username）：apply 改它要拒绝。
	ImmutableFields []string
	// NameFields 是构成 metadata.name 的字段（自然键的列，按表里的列序）；没有自然键的 kind 为空，只按 id 寻址。
	NameFields []string
	// DefaultTrueFields 是「省略即为真」的布尔字段：库默认 FALSE，但 mmwx 默认 1，没填时创建路径与 DecodeSpec 置 true。
	// 含非 spec 字段（如 User 的 is_active，动作专属），DecodeSpec 只处理其中属于 spec 的。
	DefaultTrueFields []string
	// SpecType 与 StatusType 是生成的 Spec / Status 结构体类型，explain 用它们报字段的 JSON 类型。
	SpecType   reflect.Type
	StatusType reflect.Type
	// notApplyable 是 apply 拒收清单：字段名 → 它所属的分档。spec 之外的每个字段都在这里，元数据也算。
	notApplyable map[string]string
}

// NotApplyable 返回拒收清单里的字段名，按字母序。
func (k KindInfo) NotApplyable() []string {
	fields := make([]string, 0, len(k.notApplyable))
	for f := range k.notApplyable {
		fields = append(fields, f)
	}
	sort.Strings(fields)
	return fields
}

// Immutable 报告字段是不是创建后不能改的 spec 字段：apply 更新已有对象时 MUST 拒绝改它（M1 实现）。
func (k KindInfo) Immutable(field string) bool {
	for _, f := range k.ImmutableFields {
		if f == field {
			return true
		}
	}
	return false
}

// ObjectName 从 spec（该 kind 的 Spec 结构体或其指针）算出 metadata.name：单列自然键就是那一列的值，
// 复合自然键按 NameFields 的顺序用 / 连起来（整数十进制、NULL 为空串，如 Inbound 的 3/vless-in）。
// 拼出来的 name 是展示与识别用的规范形式，值里可以含 /，所以不保证能拆回各列；按名寻址要按列查。
// 没有自然键的 kind、或传进来的不是该 kind 的 Spec 结构体（缺字段）都返回 ""、false；Object 序列化时后者会报错。
func (k KindInfo) ObjectName(spec any) (string, bool) {
	if len(k.NameFields) == 0 {
		return "", false
	}
	v := reflect.Indirect(reflect.ValueOf(spec))
	parts := make([]string, len(k.NameFields))
	for i, field := range k.NameFields {
		f, ok := fieldByJSONTag(v, field)
		if !ok {
			return "", false
		}
		parts[i] = nameValue(f)
	}
	return strings.Join(parts, "/"), true
}

func nameValue(f reflect.Value) string {
	if f.Kind() == reflect.Ptr {
		if f.IsNil() {
			return ""
		}
		f = f.Elem()
	}
	switch f.Kind() {
	case reflect.String:
		return f.String()
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return strconv.FormatInt(f.Int(), 10)
	}
	return fmt.Sprint(f.Interface())
}

// fieldByJSONTag 在结构体里按 json tag 找字段（生成的 Spec / Status 结构体每个字段都带 tag）。
func fieldByJSONTag(v reflect.Value, tag string) (reflect.Value, bool) {
	if v.Kind() != reflect.Struct {
		return reflect.Value{}, false
	}
	t := v.Type()
	for i := 0; i < t.NumField(); i++ {
		name, _, _ := strings.Cut(t.Field(i).Tag.Get("json"), ",")
		if name == tag {
			return v.Field(i), true
		}
	}
	return reflect.Value{}, false
}

// RejectReason 返回字段被 apply 拒收的分档：meta、status、action、human、master_self、readonly。
// 不在拒收清单里返回 false。
func (k KindInfo) RejectReason(field string) (string, bool) {
	class, ok := k.notApplyable[field]
	return class, ok
}

var catalog = func() map[Kind]KindInfo {
	m := make(map[Kind]KindInfo, len(generatedKinds))
	for _, k := range generatedKinds {
		m[k.Name] = k
	}
	return m
}()

// Kinds 返回全部 kind，按名字排序。
func Kinds() []KindInfo {
	kinds := make([]KindInfo, 0, len(catalog))
	for _, k := range catalog {
		kinds = append(kinds, k)
	}
	sort.Slice(kinds, func(i, j int) bool { return kinds[i].Name < kinds[j].Name })
	return kinds
}

// Lookup 按名字查 kind。
func Lookup(kind Kind) (KindInfo, bool) {
	k, ok := catalog[kind]
	return k, ok
}

// KindsOf 返回某个操作类别下的全部 kind，按名字排序。
func KindsOf(class Class) []KindInfo {
	var kinds []KindInfo
	for _, k := range Kinds() {
		if k.Class == class {
			kinds = append(kinds, k)
		}
	}
	return kinds
}

var classLabels = map[string]string{
	"meta":        "元数据",
	"status":      "status",
	"action":      "动作专属",
	"human":       "人类专属",
	"master_self": "主控自身类",
	"readonly":    "只读",
}

// DecodeSpec 按 kind 的字段清单严格解码一份 spec 到 into（该 kind 的 Spec 结构体指针）。
// 拒收清单里的字段报 field_not_applyable，不认识的字段报 unknown_field。
// spec 里没出现的「省略即为真」字段（DefaultTrueFields 里属于 spec 的）解成 true，显式写了的照写。
func DecodeSpec(kind Kind, data []byte, into any) error {
	info, ok := Lookup(kind)
	if !ok {
		return Newf(CodeBadRequest, "没有 kind %s", kind)
	}
	fields, err := decodeObject(data, "spec")
	if err != nil {
		return err
	}
	specSet := make(map[string]bool, len(info.SpecFields))
	for _, f := range info.SpecFields {
		specSet[f] = true
	}
	names := make([]string, 0, len(fields))
	for f := range fields {
		names = append(names, f)
	}
	sort.Strings(names)
	for _, f := range names {
		if class, rejected := info.notApplyable[f]; rejected {
			return Newf(CodeFieldNotApplyable, "字段 %s 是 %s 的%s字段，不能经 apply 写入", f, kind, classLabels[class])
		}
		if !specSet[f] {
			return Newf(CodeUnknownField, "%s 的 spec 没有字段 %s", kind, f)
		}
	}
	if err := json.Unmarshal(data, into); err != nil {
		return Wrap(CodeBadRequest, "spec 的字段类型不对", err)
	}
	v := reflect.Indirect(reflect.ValueOf(into))
	for _, field := range info.DefaultTrueFields {
		if !specSet[field] {
			continue
		}
		if _, present := fields[field]; present {
			continue
		}
		f, ok := fieldByJSONTag(v, field)
		if !ok || f.Kind() != reflect.Bool {
			return Newf(CodeInternal, "kind %s 的 spec 结构体里没有布尔字段 %s", kind, field)
		}
		f.SetBool(true)
	}
	return nil
}
