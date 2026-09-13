package schema

import (
	"bytes"
	"fmt"
	"go/format"
	"sort"
)

var kindClassConst = map[KindClass]string{
	KindConfig:         "ClassConfig",
	KindAction:         "ClassAction",
	KindMasterSettings: "ClassMasterSettings",
	KindSystem:         "ClassSystem",
	KindReadOnly:       "ClassReadOnly",
}

// kindTables 返回背后是 kind 的表，按 kind 名排序。
func kindTables(r *Registry) []*Table {
	var out []*Table
	for _, t := range r.Tables() {
		if t.IsKind() {
			out = append(out, t)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Kind < out[j].Kind })
	return out
}

// specColumns 返回 spec 档的列；statusColumns 返回 meta 与 spec 之外的全部列。
func specColumns(t *Table) []Column {
	var out []Column
	for _, c := range t.KindColumns() {
		if c.Class == ClassSpec {
			out = append(out, c)
		}
	}
	return out
}

func statusColumns(t *Table) []Column {
	var out []Column
	for _, c := range t.KindColumns() {
		if c.Class != ClassSpec && c.Class != ClassMeta {
			out = append(out, c)
		}
	}
	return out
}

// immutableColumns 返回创建后不能改的 spec 列。
func immutableColumns(t *Table) []Column {
	var out []Column
	for _, c := range t.KindColumns() {
		if c.Immutable {
			out = append(out, c)
		}
	}
	return out
}

// nameColumns 返回构成 metadata.name 的列：全部自然键索引的列的并集，按表里的列序；没有自然键为空。
func nameColumns(t *Table) []Column {
	in := map[string]bool{}
	for _, ix := range t.Indexes {
		if ix.NaturalKey {
			for _, c := range ix.Columns {
				in[c] = true
			}
		}
	}
	var out []Column
	for _, c := range t.KindColumns() {
		if in[c.Name] {
			out = append(out, c)
		}
	}
	return out
}

// defaultTrueColumns 返回标了「省略即为真」的列（含非 spec 列，创建路径也要查它）。
func defaultTrueColumns(t *Table) []Column {
	var out []Column
	for _, c := range t.KindColumns() {
		if c.DefaultTrue {
			out = append(out, c)
		}
	}
	return out
}

// maskedColumns 返回标了打码的列（不含 meta）。
func maskedColumns(t *Table) []Column {
	var out []Column
	for _, c := range t.KindColumns() {
		if c.Masked && c.Class != ClassMeta {
			out = append(out, c)
		}
	}
	return out
}

// kindGoType 是列在 Spec / Status 结构体里的类型：打码列用 Secret / SecretJSON，序列化默认打码，
// 忘了打码的输出路径也写不出原文；其它列与 bun 模型同型。
func kindGoType(c Column) string {
	if !c.Masked {
		return goType(c)
	}
	switch c.Type {
	case TypeJSON:
		return "SecretJSON"
	case TypeText:
		if c.Nullable {
			return "*Secret"
		}
		return "Secret"
	}
	return goType(c)
}

func writeStruct(b *bytes.Buffer, name string, cols []Column) {
	if len(cols) == 0 {
		fmt.Fprintf(b, "type %s struct{}\n\n", name)
		return
	}
	fmt.Fprintf(b, "type %s struct {\n", name)
	for _, c := range cols {
		fmt.Fprintf(b, "\t%s %s `json:\"%s\"`\n", GoName(c.Name), kindGoType(c), c.Name)
	}
	b.WriteString("}\n\n")
}

func writeStringSlice(b *bytes.Buffer, cols []Column) {
	b.WriteString("[]string{")
	for i, c := range cols {
		if i > 0 {
			b.WriteString(", ")
		}
		fmt.Fprintf(b, "%q", c.Name)
	}
	b.WriteString("}")
}

// GenerateKinds 生成 pkg/api/v1/zz_generated_kinds.go：每个 kind 的 Spec / Status 结构体与 kind 清单。
func GenerateKinds(r *Registry) ([]byte, error) {
	kinds := kindTables(r)
	// import 只按会出现在 Spec / Status 结构体里的字段算（列加键值表的 key），meta 列不在其中。
	var emitted []Column
	for _, t := range kinds {
		emitted = append(emitted, specColumns(t)...)
		emitted = append(emitted, statusColumns(t)...)
	}
	var b bytes.Buffer
	b.WriteString(generatedHeader + "\npackage v1\n\n")
	writeImports(&b, emitted)
	for _, t := range kinds {
		fmt.Fprintf(&b, "// %sSpec 是 kind %s（%s）的 spec 字段。\n", t.Kind, t.Kind, t.KindClass)
		writeStruct(&b, t.Kind+"Spec", specColumns(t))
		fmt.Fprintf(&b, "// %sStatus 是 kind %s 的 status 字段：status、动作专属、人类专属、主控自身类与只读，apply 一律拒收。\n", t.Kind, t.Kind)
		writeStruct(&b, t.Kind+"Status", statusColumns(t))
	}
	b.WriteString("// generatedKinds 是 kind 清单，Kinds、Lookup、KindsOf 与 DecodeSpec 都查它。\n")
	b.WriteString("var generatedKinds = []KindInfo{\n")
	for _, t := range kinds {
		fmt.Fprintf(&b, "\t{\n\t\tName: %q,\n\t\tClass: %s,\n", t.Kind, kindClassConst[t.KindClass])
		b.WriteString("\t\tSpecFields: ")
		writeStringSlice(&b, specColumns(t))
		b.WriteString(",\n\t\tStatusFields: ")
		writeStringSlice(&b, statusColumns(t))
		b.WriteString(",\n\t\tMaskedFields: ")
		writeStringSlice(&b, maskedColumns(t))
		b.WriteString(",\n\t\tImmutableFields: ")
		writeStringSlice(&b, immutableColumns(t))
		b.WriteString(",\n\t\tNameFields: ")
		writeStringSlice(&b, nameColumns(t))
		b.WriteString(",\n\t\tDefaultTrueFields: ")
		writeStringSlice(&b, defaultTrueColumns(t))
		b.WriteString(",\n\t\tnotApplyable: map[string]string{")
		first := true
		for _, c := range t.KindColumns() {
			if c.Class == ClassSpec {
				continue
			}
			if !first {
				b.WriteString(", ")
			}
			first = false
			fmt.Fprintf(&b, "%q: %q", c.Name, c.Class.String())
		}
		b.WriteString("},\n\t},\n")
	}
	b.WriteString("}\n")
	return format.Source(b.Bytes())
}
