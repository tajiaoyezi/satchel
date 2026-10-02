package cli

import (
	"sort"
	"strings"
	"testing"

	"github.com/spf13/pflag"

	"github.com/satchel/satchel/internal/command"
)

// pflagType 是命令表里的 flag 类型在 cobra 上登记成的 pflag 类型（registerFlag 与 root.go）。
func pflagType(t command.FlagType) string {
	switch t {
	case command.TypeBool:
		return "bool"
	case command.TypeInt:
		return "int"
	case command.TypeDuration:
		return "duration"
	case command.TypeStrings, command.TypeObject:
		return "stringArray"
	}
	return "string" // string、file
}

// command.CLIFlags 是「一条命令在 CLI 上接受哪些 flag」的唯一来源（skills 的检查按它认 flag）：
// 对每一条命令，cobra 树上实际的 flag（本地的与继承的，名字与类型）等于它推出的，再加上自带的 help（-h）。
// 唯一的例外：不会连主控的本地命令也从根上继承了 server、token，比对时去掉——它们在执行时按同一份清单以 usage 拒绝
// （TestLocalCommandsRejectConnectionFlags）。
func TestCLIFlagsMatchCommandTable(t *testing.T) {
	opts := testOptions()
	root := NewRoot(opts)
	for _, c := range opts.Table.All() {
		leaf, _, err := root.Find(c.Path)
		if err != nil || leaf.Annotations[annotationName] != c.Name() {
			t.Fatalf("%s 在 cobra 树上找不到：%v", c.Name(), err)
		}
		leaf.InitDefaultHelpFlag()
		got := map[string]string{}
		collect := func(f *pflag.Flag) {
			got[f.Name] = f.Value.Type()
			if f.Name == "help" && f.Shorthand != "h" {
				t.Errorf("%s 的 --help 应当带短名 -h，得到 %q", c.Name(), f.Shorthand)
			}
		}
		leaf.LocalFlags().VisitAll(collect)
		leaf.InheritedFlags().VisitAll(collect)
		if c.Class == command.ClassLocal && !command.IsConnectingLocal(c.Name()) {
			delete(got, "server")
			delete(got, "token")
		}
		want := map[string]string{"help": "bool"}
		for _, f := range command.CLIFlags(c) {
			want[f.Name] = pflagType(f.Type)
		}
		if diff := flagDiff(want, got); diff != "" {
			t.Errorf("%s 的 flag 与 command.CLIFlags 不一致：%s", c.Name(), diff)
		}
	}
}

func flagDiff(want, got map[string]string) string {
	var out []string
	for name, typ := range want {
		if g, ok := got[name]; !ok {
			out = append(out, "缺 --"+name)
		} else if g != typ {
			out = append(out, "--"+name+" 应为 "+typ+"，得到 "+g)
		}
	}
	for name := range got {
		if _, ok := want[name]; !ok {
			out = append(out, "多了 --"+name)
		}
	}
	sort.Strings(out)
	return strings.Join(out, "；")
}

// 根 flag 按 command.RootFlags 登记，ClientOnlyFlags 由它推出且仍是 server、token、data-dir 三个（MCP 照旧按它拒绝，--json 照常接受）。
func TestRootFlagsFromCommandTable(t *testing.T) {
	root := NewRoot(testOptions())
	for _, f := range command.RootFlags {
		pf := root.PersistentFlags().Lookup(f.Name)
		if pf == nil || pf.Value.Type() != pflagType(f.Type) {
			t.Errorf("根 flag --%s 应当按 RootFlags 登记成 %s：%v", f.Name, pflagType(f.Type), pf)
		}
	}
	n := 0
	root.PersistentFlags().VisitAll(func(*pflag.Flag) { n++ })
	if n != len(command.RootFlags) {
		t.Errorf("根上的 persistent flag 有 %d 个，RootFlags 有 %d 个", n, len(command.RootFlags))
	}
	got := strings.Join(command.ClientOnlyFlags, ",")
	if got != "server,token,data-dir" {
		t.Errorf("ClientOnlyFlags 应当仍是 server、token、data-dir：%s", got)
	}
}
