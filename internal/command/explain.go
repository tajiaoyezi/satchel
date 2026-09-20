package command

import (
	"encoding/json"
	"reflect"
	"strings"
	"time"

	v1 "github.com/satchel/satchel/pkg/api/v1"
)

// explain 的输出形状。它只读命令表与 kind 清单，纯函数、无 IO，CLI 本地作答、REST 与 MCP 直接调（master-mcp「explain 工具」）。

// Overview 是不带 target 时的输出：全部命令与全部 kind。
type Overview struct {
	Commands []CommandInfo `json:"commands"`
	Kinds    []KindSummary `json:"kinds"`
}

// CommandInfo 是一条命令的说明。
type CommandInfo struct {
	Name      string     `json:"name"`
	Summary   string     `json:"summary"`
	Class     Class      `json:"class"`
	Scope     string     `json:"scope"`
	Danger    string     `json:"danger"`
	Confirm   *Confirm   `json:"confirm,omitempty"`
	HumanOnly bool       `json:"human_only"`
	Anonymous bool       `json:"anonymous"`
	List      bool       `json:"list"`
	Offline   bool       `json:"offline"`
	Args      []ArgInfo  `json:"args"`
	Flags     []FlagInfo `json:"flags"`
	REST      *RESTInfo  `json:"rest,omitempty"`
	// Reserved 是投影层随类别自动加的 flag：列表命令的 limit / cursor，危险命令的 confirm。
	Reserved []string `json:"reserved_flags"`
}

// ArgInfo 是位置参数的说明。
type ArgInfo struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Optional    bool   `json:"optional"`
}

// FlagInfo 是 flag 的说明。Kind 只有 object 类型有：字段名只能是该 kind 的字段（explain <kind> 看得到清单）。
type FlagInfo struct {
	Name        string   `json:"name"`
	Type        FlagType `json:"type"`
	Default     string   `json:"default,omitempty"`
	Description string   `json:"description"`
	Secret      bool     `json:"secret,omitempty"`
	Kind        v1.Kind  `json:"kind,omitempty"`
}

// RESTInfo 是命令的 REST 映射。
type RESTInfo struct {
	Method string `json:"method"`
	Path   string `json:"path"`
}

// KindSummary 是 kind 在总览里的一行。
type KindSummary struct {
	Name         v1.Kind  `json:"name"`
	Class        v1.Class `json:"class"`
	SpecFields   int      `json:"spec_fields"`
	StatusFields int      `json:"status_fields"`
}

// KindExplain 是一个 kind 的字段说明。
type KindExplain struct {
	Name       v1.Kind     `json:"name"`
	Class      v1.Class    `json:"class"`
	NameFields []string    `json:"name_fields"`
	Fields     []FieldInfo `json:"fields"`
}

// FieldInfo 是 kind 的一个字段：名字、JSON 类型、分档（spec 或拒收清单里的档），以及打码、不可改、省略即为真三个标记。
type FieldInfo struct {
	Name        string `json:"name"`
	Type        string `json:"type"`
	Nullable    bool   `json:"nullable"`
	Tier        string `json:"tier"`
	Masked      bool   `json:"masked,omitempty"`
	Immutable   bool   `json:"immutable,omitempty"`
	DefaultTrue bool   `json:"default_true,omitempty"`
}

// Explain 解释一个 target：空串给总览，命令路径给 CommandInfo，kind 名给 KindExplain，都不是报 not_found。
// 命令路径的各段可以用空格连（"audit list"）。
func Explain(t *Table, target string) (any, error) {
	target = strings.TrimSpace(target)
	if target == "" {
		return overview(t), nil
	}
	if c, ok := t.Lookup(target); ok {
		return Describe(c), nil
	}
	if k, ok := v1.Lookup(v1.Kind(target)); ok {
		return describeKind(k), nil
	}
	return nil, v1.Newf(v1.CodeNotFound, "没有叫 %s 的命令或 kind", target).
		WithNext("运行 satchel explain 查看全部命令与 kind")
}

func overview(t *Table) Overview {
	o := Overview{Commands: []CommandInfo{}, Kinds: []KindSummary{}}
	for _, c := range t.All() {
		if c.Hidden {
			continue
		}
		o.Commands = append(o.Commands, Describe(c))
	}
	for _, k := range v1.Kinds() {
		o.Kinds = append(o.Kinds, KindSummary{Name: k.Name, Class: k.Class, SpecFields: len(k.SpecFields), StatusFields: len(k.StatusFields)})
	}
	return o
}

// Describe 是一条命令的说明，docs/commands.md 的生成器也用它。
func Describe(c *Command) CommandInfo {
	info := CommandInfo{
		Name: c.Name(), Summary: c.Summary, Class: c.Class, Danger: string(c.Danger), Confirm: c.Confirm,
		HumanOnly: c.HumanOnly, Anonymous: c.Anonymous, List: c.List, Offline: c.Offline, Args: []ArgInfo{}, Flags: []FlagInfo{}, Reserved: []string{},
	}
	if scope, ok := c.Scope(); ok {
		info.Scope = string(scope)
	}
	for _, a := range c.Args {
		info.Args = append(info.Args, ArgInfo{Name: a.Name, Description: a.Description, Optional: a.Optional})
	}
	for _, f := range c.Flags {
		info.Flags = append(info.Flags, FlagInfo{Name: f.Name, Type: f.Type, Default: f.Default, Description: f.Description, Secret: f.Secret, Kind: f.Kind})
	}
	if c.Class != ClassLocal {
		r := c.Route()
		info.REST = &RESTInfo{Method: r.Method, Path: r.Path}
	}
	if c.List {
		info.Reserved = append(info.Reserved, "limit", "cursor")
	}
	if c.Danger != "" {
		info.Reserved = append(info.Reserved, "confirm")
	}
	if c.HumanOnly {
		info.Reserved = append(info.Reserved, VerifyFlags...)
	}
	return info
}

func describeKind(k v1.KindInfo) KindExplain {
	out := KindExplain{Name: k.Name, Class: k.Class, NameFields: append([]string{}, k.NameFields...), Fields: []FieldInfo{}}
	for _, name := range k.SpecFields {
		f := fieldInfo(k.SpecType, name)
		f.Tier = "spec"
		f.Masked = contains(k.MaskedFields, name)
		f.Immutable = contains(k.ImmutableFields, name)
		f.DefaultTrue = contains(k.DefaultTrueFields, name)
		out.Fields = append(out.Fields, f)
	}
	for _, name := range k.StatusFields {
		f := fieldInfo(k.StatusType, name)
		if tier, ok := k.RejectReason(name); ok {
			f.Tier = tier
		} else {
			f.Tier = "status"
		}
		f.Masked = contains(k.MaskedFields, name)
		f.DefaultTrue = contains(k.DefaultTrueFields, name)
		out.Fields = append(out.Fields, f)
	}
	return out
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

var (
	timeType       = reflect.TypeOf(time.Time{})
	rawJSONType    = reflect.TypeOf(json.RawMessage{})
	secretType     = reflect.TypeOf(v1.Secret(""))
	secretJSONType = reflect.TypeOf(v1.SecretJSON{})
)

// fieldInfo 用反射从生成的结构体读字段的 JSON 类型；结构体里找不到（不该发生）就只给名字。
func fieldInfo(st reflect.Type, name string) FieldInfo {
	info := FieldInfo{Name: name, Type: "unknown"}
	if st == nil {
		return info
	}
	for i := 0; i < st.NumField(); i++ {
		tag, _, _ := strings.Cut(st.Field(i).Tag.Get("json"), ",")
		if tag != name {
			continue
		}
		ft := st.Field(i).Type
		if ft.Kind() == reflect.Ptr {
			info.Nullable = true
			ft = ft.Elem()
		}
		info.Type = jsonType(ft)
		return info
	}
	return info
}

func jsonType(t reflect.Type) string {
	switch t {
	case timeType:
		return "string (RFC 3339 time)"
	case rawJSONType:
		return "json"
	case secretType:
		return "string (masked)"
	case secretJSONType:
		return "json (masked)"
	}
	switch t.Kind() {
	case reflect.String:
		return "string"
	case reflect.Bool:
		return "boolean"
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64, reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return "integer"
	case reflect.Float32, reflect.Float64:
		return "number"
	case reflect.Slice, reflect.Array:
		return "array"
	case reflect.Map, reflect.Struct:
		return "object"
	}
	return t.String()
}
