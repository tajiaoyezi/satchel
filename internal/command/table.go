package command

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

// Table 是一张校验过的命令表。New 失败即进程不该启动。
type Table struct {
	cmds   []*Command
	byName map[string]*Command
}

// New 用给出的命令建表并做全表校验：单条规则见 Command.validate；跨条规则是路径唯一、
// 一条路径不能是另一条的前缀（有 audit list 就不能再有 audit，父节点只做分组）。cobra 的别名不在类型里，
// 表因此天然没有别名——投影层也不许自己加。
func New(cmds ...*Command) (*Table, error) {
	t := &Table{byName: make(map[string]*Command, len(cmds))}
	var errs []error
	for _, c := range cmds {
		if err := c.validate(); err != nil {
			errs = append(errs, err)
			continue
		}
		name := c.Name()
		if _, dup := t.byName[name]; dup {
			errs = append(errs, fmt.Errorf("命令路径 %q 重复", name))
			continue
		}
		t.byName[name] = c
		t.cmds = append(t.cmds, c)
	}
	sort.Slice(t.cmds, func(i, j int) bool { return t.cmds[i].Name() < t.cmds[j].Name() })
	for i, a := range t.cmds {
		for _, b := range t.cmds[i+1:] {
			if isPrefix(a.Path, b.Path) {
				errs = append(errs, fmt.Errorf("命令路径 %q 是 %q 的前缀：父节点只做分组，不能同时是命令", a.Name(), b.Name()))
			}
		}
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return t, nil
}

// MustNew 是 New 的 panic 版，给编译期就该正确的目录用。
func MustNew(cmds ...*Command) *Table {
	t, err := New(cmds...)
	if err != nil {
		panic("命令表不合法：" + err.Error())
	}
	return t
}

func isPrefix(short, long []string) bool {
	if len(short) >= len(long) {
		return false
	}
	for i := range short {
		if short[i] != long[i] {
			return false
		}
	}
	return true
}

// GroupSummary 返回分组节点（多段路径的中间段，如 db、audit）在帮助里的一句话；没登记的按段名生成。
func (t *Table) GroupSummary(name string) string {
	if s, ok := GroupSummaries[name]; ok {
		return s
	}
	return name + " 相关命令"
}

// Lookup 按名字（各段用空格连接）找命令。
func (t *Table) Lookup(name string) (*Command, bool) {
	c, ok := t.byName[strings.Join(strings.Fields(name), " ")]
	return c, ok
}

// All 返回全部命令，按名字排序。
func (t *Table) All() []*Command {
	return append([]*Command(nil), t.cmds...)
}

// Remote 返回全部非本地命令（要经主控执行的），按名字排序。
func (t *Table) Remote() []*Command {
	var out []*Command
	for _, c := range t.cmds {
		if c.Class != ClassLocal {
			out = append(out, c)
		}
	}
	return out
}

// Local 返回全部本地命令，按名字排序。
func (t *Table) Local() []*Command {
	var out []*Command
	for _, c := range t.cmds {
		if c.Class == ClassLocal {
			out = append(out, c)
		}
	}
	return out
}

// HumanOnly 返回全部人类专属命令的名字：MCP 的拒绝清单。
func (t *Table) HumanOnly() []string {
	var out []string
	for _, c := range t.cmds {
		if c.HumanOnly {
			out = append(out, c.Name())
		}
	}
	return out
}

// Names 返回全部命令名。
func (t *Table) Names() []string {
	out := make([]string, 0, len(t.cmds))
	for _, c := range t.cmds {
		out = append(out, c.Name())
	}
	return out
}

// CheckBindings 校验绑定与表一一对应：表里每条非本地命令都有处理函数（离线命令也要，REST 与 MCP 经执行链调它；
// 只有 CLI 本地作答），绑定里没有表外的名字。装配（cmd/satchel）在启动时调它，端到端测试也调。
func (t *Table) CheckBindings(b Bindings) error {
	var errs []error
	for _, c := range t.Remote() {
		if _, ok := b[c.Name()]; !ok {
			errs = append(errs, fmt.Errorf("命令 %q 没有绑定处理函数", c.Name()))
		}
	}
	for name := range b {
		if _, ok := t.byName[name]; !ok {
			errs = append(errs, fmt.Errorf("绑定里的 %q 不在命令表里", name))
		}
	}
	return errors.Join(errs...)
}
