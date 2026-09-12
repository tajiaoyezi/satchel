package v1

import (
	"encoding/json"
	"sort"
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
	return nil
}
