package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/satchel/satchel/internal/command"
	v1 "github.com/satchel/satchel/pkg/api/v1"
)

// render 输出一条命令的结果：--json 时是带 apiVersion 的 JSON 对象（与 REST、MCP 同一份），
// 否则按命令名找文本渲染，没登记的用通用渲染。
func render(w io.Writer, c *command.Command, result any, opts Options, o *options) error {
	if o.json {
		return writeJSON(w, result)
	}
	if r, ok := opts.Renderers[c.Name()]; ok {
		return r(w, result)
	}
	return renderGeneric(w, c, result)
}

// writeJSON 把载荷编码成一个 JSON 对象写出，并注入 apiVersion。
func writeJSON(w io.Writer, payload any) error {
	out, err := v1.MarshalOutput(payload)
	if err != nil {
		return err
	}
	_, err = w.Write(out)
	return err
}

// renderGeneric 是通用文本渲染：列表结果画表（列取命令登记的 Columns，没登记就用第一条的键排序），
// 资源信封（apiVersion / kind / metadata / spec / status）分 metadata、spec、status 三段逐字段列出，
// 其它结果按 JSON 键排序逐行「键：值」。
func renderGeneric(w io.Writer, c *command.Command, result any) error {
	fields, err := toFields(result)
	if err != nil {
		return err
	}
	if items, ok := fields["items"]; ok {
		if _, isList := items.([]any); isList {
			return renderPage(w, c, fields)
		}
	}
	if isEnvelope(fields) {
		return renderEnvelope(w, fields)
	}
	return renderFields(w, fields)
}

func isEnvelope(fields map[string]any) bool {
	for _, k := range []string{"apiVersion", "kind", "metadata", "spec", "status"} {
		if _, ok := fields[k]; !ok {
			return false
		}
	}
	return true
}

// renderEnvelope 把一个资源对象分三段打印：第一行是 kind，然后 metadata、spec、status 各一段，段内按键排序。
func renderEnvelope(w io.Writer, fields map[string]any) error {
	if _, err := fmt.Fprintf(w, "kind：%s\n", formatValue(fields["kind"])); err != nil {
		return err
	}
	for _, section := range []string{"metadata", "spec", "status"} {
		if _, err := fmt.Fprintf(w, "\n[%s]\n", section); err != nil {
			return err
		}
		sub, _ := fields[section].(map[string]any)
		if err := renderFields(w, sub); err != nil {
			return err
		}
	}
	return nil
}

// toFields 经 JSON 往返把任何结果变成 map，渲染只认 JSON 里的样子（与 --json 看到的一致）。
func toFields(result any) (map[string]any, error) {
	raw, err := json.Marshal(result)
	if err != nil {
		return nil, v1.Wrap(v1.CodeInternal, "编码输出失败", err)
	}
	var fields map[string]any
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, v1.Wrap(v1.CodeInternal, "输出必须是对象", err)
	}
	return fields, nil
}

func renderFields(w io.Writer, fields map[string]any) error {
	keys := make([]string, 0, len(fields))
	for k := range fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if _, err := fmt.Fprintf(w, "%s：%s\n", k, formatValue(fields[k])); err != nil {
			return err
		}
	}
	return nil
}

func renderPage(w io.Writer, c *command.Command, fields map[string]any) error {
	items, _ := fields["items"].([]any)
	cols := c.Columns
	if len(cols) == 0 && len(items) > 0 {
		if first, ok := items[0].(map[string]any); ok {
			for k := range first {
				cols = append(cols, k)
			}
			sort.Strings(cols)
		}
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, strings.ToUpper(strings.Join(cols, "\t")))
	for _, item := range items {
		row, _ := item.(map[string]any)
		cells := make([]string, len(cols))
		for i, col := range cols {
			cells[i] = formatValue(row[col])
		}
		fmt.Fprintln(tw, strings.Join(cells, "\t"))
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	total, _ := fields["total"].(float64)
	next, _ := fields["nextCursor"].(string)
	if next != "" {
		_, err := fmt.Fprintf(w, "共 %d 条，还有下一页：--cursor %s\n", int(total), next)
		return err
	}
	_, err := fmt.Fprintf(w, "共 %d 条\n", int(total))
	return err
}

func formatValue(v any) string {
	switch x := v.(type) {
	case nil:
		return "-"
	case string:
		return x
	case float64:
		if x == float64(int64(x)) {
			return fmt.Sprintf("%d", int64(x))
		}
		return fmt.Sprintf("%g", x)
	case bool:
		if x {
			return "true"
		}
		return "false"
	case []any:
		parts := make([]string, len(x))
		for i, item := range x {
			parts[i] = formatValue(item)
		}
		return strings.Join(parts, ", ")
	}
	raw, _ := json.Marshal(v)
	return string(raw)
}

// renderExplain 是 explain 的文本形式：命令总览、单条命令或一个 kind 的字段表。
func renderExplain(w io.Writer, result any) error {
	switch x := result.(type) {
	case command.Overview:
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "命令\t类别\tscope\t说明")
		for _, c := range x.Commands {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", c.Name, c.Class, orDash(c.Scope), c.Summary)
		}
		fmt.Fprintln(tw, "\nkind\t类别\tspec 字段\tstatus 字段")
		for _, k := range x.Kinds {
			fmt.Fprintf(tw, "%s\t%s\t%d\t%d\n", k.Name, k.Class, k.SpecFields, k.StatusFields)
		}
		return tw.Flush()
	case command.CommandInfo:
		fmt.Fprintf(w, "%s：%s\n类别：%s\tscope：%s\t危险类：%s\t人类专属：%v\n", x.Name, x.Summary, x.Class, orDash(x.Scope), orDash(x.Danger), x.HumanOnly)
		if x.REST != nil {
			fmt.Fprintf(w, "REST：%s %s\n", x.REST.Method, x.REST.Path)
		}
		if x.Confirm != nil {
			fmt.Fprintf(w, "confirm：%s %s\n", x.Confirm.Kind, x.Confirm.Arg)
		}
		for _, a := range x.Args {
			opt := ""
			if a.Optional {
				opt = "（可选）"
			}
			fmt.Fprintf(w, "参数 %s%s：%s\n", a.Name, opt, a.Description)
		}
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		for _, f := range x.Flags {
			fmt.Fprintf(tw, "--%s\t%s\t%s\n", f.Name, f.Type, f.Description)
		}
		for _, r := range x.Reserved {
			fmt.Fprintf(tw, "--%s\t\t（随类别自动加）\n", r)
		}
		return tw.Flush()
	case command.KindExplain:
		fmt.Fprintf(w, "kind %s（%s）\t名字字段：%s\n", x.Name, x.Class, orDash(strings.Join(x.NameFields, "/")))
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "字段\t类型\t分档\t标记")
		for _, f := range x.Fields {
			var marks []string
			if f.Nullable {
				marks = append(marks, "可空")
			}
			if f.Masked {
				marks = append(marks, "打码")
			}
			if f.Immutable {
				marks = append(marks, "不可改")
			}
			if f.DefaultTrue {
				marks = append(marks, "省略即为真")
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", f.Name, f.Type, f.Tier, strings.Join(marks, " "))
		}
		return tw.Flush()
	}
	fields, err := toFields(result)
	if err != nil {
		return err
	}
	return renderFields(w, fields)
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
