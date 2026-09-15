// 分层守卫：按第 03 章的六层检查整个仓库的 import 图，依赖只能向下。
// 规则表在 layerOf 与 checkLayering 里；新目录若不在表里，测试直接失败，逼着新代码进树。
package internal

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"sort"
	"strings"
	"testing"
)

const module = "github.com/satchel/satchel"

// layer 的数值越大越靠下；只允许引用数值不小于自己的层。
type layer int

const (
	layerCmd        layer = iota // 装配根，可引用任何层
	layerProjection              // 投影
	layerMiddleware              // 横切
	layerService                 // 业务
	layerCore                    // 仓储
	layerBase                    // 基础设施
	layerContract                // 契约，任何层都可引用
)

var layerNames = [...]string{"cmd", "projection", "middleware", "service", "core", "base", "contract"}

func (l layer) String() string { return layerNames[l] }

// layerOf 返回包所属的层；不在六层目录里返回 ok=false。
// 契约层是 pkg/ 下的全部包（api/v1 之外还有与 satchel-agent 共用的 securechan、xrpc 等，第 02 章）加 internal/command。
func layerOf(pkg string) (l layer, ok bool) {
	rel := strings.TrimPrefix(pkg, module+"/")
	switch {
	case strings.HasPrefix(rel, "pkg/"), rel == "internal/command":
		return layerContract, true
	case strings.HasPrefix(rel, "cmd/"), strings.HasPrefix(rel, "tools/"):
		return layerCmd, true // 装配与开发工具（如发布签名程序 tools/sign），可引用任何层
	case strings.HasPrefix(rel, "internal/projection/"):
		return layerProjection, true
	case strings.HasPrefix(rel, "internal/middleware/"):
		return layerMiddleware, true
	case rel == "internal/service", strings.HasPrefix(rel, "internal/service/"):
		return layerService, true
	case rel == "internal/core", strings.HasPrefix(rel, "internal/core/"):
		return layerCore, true
	case strings.HasPrefix(rel, "internal/base/"):
		return layerBase, true
	}
	return 0, false
}

// checkLayering 检查 import 图（包 → 它引用的包），返回违规清单；空表示合规。
func checkLayering(imports map[string][]string) []string {
	var violations []string
	for pkg, deps := range imports {
		if pkg == module+"/internal" { // 本测试所在的目录，只有测试文件
			continue
		}
		from, ok := layerOf(pkg)
		if !ok {
			violations = append(violations, pkg+": 不在六层目录里")
			continue
		}
		for _, dep := range deps {
			if !strings.HasPrefix(dep, module+"/") {
				continue // 标准库与第三方包不管
			}
			to, ok := layerOf(dep)
			if !ok {
				continue // 目标包自己那一轮会被报
			}
			switch {
			case from == layerContract:
				// 契约包之间可以互引（xrpc 用 securechan、都用 api/v1，internal/command 用 pkg/*），但不能碰 internal/ 下别的包：
				// pkg/ 被 satchel-agent 以 module 引用，带上 internal 就把主控的实现拖过去了。
				if strings.HasPrefix(dep, module+"/pkg/") {
					continue
				}
				violations = append(violations, fmt.Sprintf("%s → %s: 契约包只能引用 pkg/ 下的包", pkg, dep))
			case to < from:
				violations = append(violations, fmt.Sprintf("%s → %s: %s 层不能引用 %s 层", pkg, dep, from, to))
			case from == layerCore && to == layerCore && pkg != dep:
				violations = append(violations, fmt.Sprintf("%s → %s: core 模块之间不能互相引用", pkg, dep))
			}
		}
	}
	sort.Strings(violations)
	return violations
}

// loadImports 用 go list 取整个仓库的 import 图，测试文件的引用也算在内。
func loadImports(t *testing.T) map[string][]string {
	t.Helper()
	cmd := exec.Command("go", "list", "-json=ImportPath,Imports,TestImports,XTestImports", "./...")
	cmd.Dir = ".."
	out, err := cmd.Output()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			t.Fatalf("go list 失败：%v\n%s", err, exitErr.Stderr)
		}
		t.Fatalf("go list 失败：%v", err)
	}
	imports := map[string][]string{}
	dec := json.NewDecoder(bytes.NewReader(out))
	for {
		var p struct {
			ImportPath   string
			Imports      []string
			TestImports  []string
			XTestImports []string
		}
		if err := dec.Decode(&p); err == io.EOF {
			break
		} else if err != nil {
			t.Fatalf("解析 go list 输出失败：%v", err)
		}
		deps := append(append(append([]string{}, p.Imports...), p.TestImports...), p.XTestImports...)
		imports[p.ImportPath] = deps
	}
	return imports
}

func TestLayering(t *testing.T) {
	imports := loadImports(t)
	if len(imports) == 0 {
		t.Fatal("go list 没有列出任何包")
	}
	if v := checkLayering(imports); len(v) > 0 {
		t.Fatalf("分层违规 %d 处：\n  %s", len(v), strings.Join(v, "\n  "))
	}
}

func TestCheckLayeringCatchesViolations(t *testing.T) {
	p := func(rel string) string { return module + "/" + rel }
	cases := []struct {
		name    string
		imports map[string][]string
		want    string // 违规信息里应当出现的片段
	}{
		{"base 引用 service", map[string][]string{p("internal/base/db"): {p("internal/service/users")}}, "base 层不能引用 service 层"},
		{"service 引用 projection", map[string][]string{p("internal/service/users"): {p("internal/projection/rest")}}, "service 层不能引用 projection 层"},
		{"core 互引", map[string][]string{p("internal/core/users"): {p("internal/core/packages")}}, "core 模块之间不能互相引用"},
		{"pkg 引用 internal", map[string][]string{p("pkg/api/v1"): {p("internal/base/model")}}, "契约包只能引用 pkg/ 下的包"},
		{"共用包引用 internal", map[string][]string{p("pkg/securechan"): {p("internal/base/agentconn")}}, "契约包只能引用 pkg/ 下的包"},
		{"共用包引用 command", map[string][]string{p("pkg/xrpc"): {p("internal/command")}}, "契约包只能引用 pkg/ 下的包"},
		{"command 引用 base", map[string][]string{p("internal/command"): {p("internal/base/schema")}}, "契约包只能引用 pkg/ 下的包"},
		{"不在六层里的包", map[string][]string{p("internal/util"): nil}, "不在六层目录里"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := checkLayering(tc.imports)
			if len(got) != 1 || !strings.Contains(got[0], tc.want) {
				t.Fatalf("想要一条含 %q 的违规，得到 %v", tc.want, got)
			}
		})
	}
}

func TestCheckLayeringAcceptsCompliantGraph(t *testing.T) {
	p := func(rel string) string { return module + "/" + rel }
	imports := map[string][]string{
		p("cmd/satchel"):               {p("internal/projection/cli"), p("internal/base/db")},
		p("internal/projection/cli"):   {p("internal/middleware/authz"), p("internal/service/users"), p("pkg/api/v1"), "github.com/spf13/cobra"},
		p("internal/projection/mcp"):   {p("internal/projection/cli")},
		p("internal/middleware/authz"): {p("internal/command"), p("internal/service/tokens")},
		p("internal/service/users"):    {p("internal/core/users"), p("internal/core/packages"), p("internal/service/plan")},
		p("internal/core/users"):       {p("internal/base/model"), p("internal/base/store")},
		p("internal/base/store"):       {p("internal/base/model"), "database/sql"},
		p("internal/command"):          {p("pkg/api/v1")},
		p("pkg/api/v1"):                {"encoding/json"},
		p("pkg/securechan"):            {p("pkg/api/v1"), "crypto/ed25519"},
		p("pkg/xrpc"):                  {p("pkg/securechan"), p("pkg/api/v1")},
		p("internal/base/agentconn"):   {p("pkg/xrpc")},
		p("internal"):                  {"testing", "os/exec"},
	}
	if got := checkLayering(imports); len(got) != 0 {
		t.Fatalf("合规的 import 图不该有违规，得到 %v", got)
	}
}
