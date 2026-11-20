package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/satchel/satchel/internal/command"
	"github.com/satchel/satchel/internal/projection/rest"
	v1 "github.com/satchel/satchel/pkg/api/v1"
)

func planApplyTable(t *testing.T) *command.Table {
	t.Helper()
	tbl, err := command.New(
		&command.Command{
			Path: []string{"plan"}, Summary: "预演", Class: command.ClassConfig,
			Flags: []command.Flag{
				{Name: "spec", Type: command.TypeString, Description: "内联 JSON"},
				{Name: "file", Type: command.TypeFile, Description: "本地 JSON 文件"},
			},
		},
		&command.Command{
			Path: []string{"apply"}, Summary: "提交", Class: command.ClassConfig, DynamicDanger: true,
			Flags: []command.Flag{
				{Name: "plan", Type: command.TypeInt, Description: "计划编号"},
				{Name: "force", Type: command.TypeBool, Description: "跳过版本校验"},
			},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	return tbl
}

func TestCLIInlineFileFlagAndDynamicConfirm(t *testing.T) {
	tbl := planApplyTable(t)
	var gotInv *command.Invocation
	srv := httptest.NewServer(rest.NewHandler(tbl, command.RunnerFunc(func(_ context.Context, inv *command.Invocation) (any, error) {
		gotInv = inv
		return map[string]any{"command": inv.Name(), "spec": inv.String("spec", ""), "confirm": inv.Confirm, "force": inv.Bool("force")}, nil
	}), nil))
	defer srv.Close()

	// 1. --file 读取本地文件并内联到 spec，请求体不含 file，经真实 REST 往返验证。
	dir := t.TempDir()
	specPath := filepath.Join(dir, "change.json")
	wantSpec := `{"op":"create","kind":"Server","spec":{"name":"hk-1"}}`
	if err := os.WriteFile(specPath, []byte(wantSpec), 0o600); err != nil {
		t.Fatal(err)
	}
	opts := Options{Table: tbl, Remote: func(string) command.Runner {
		return NewClient(tbl, Connection{Server: srv.URL})
	}}
	var stdout, stderr bytes.Buffer
	code := Execute(opts, []string{"plan", "--file", specPath, "--json"}, &stdout, &stderr)
	if code != v1.ExitOK {
		t.Fatalf("plan --file 失败 code=%d stderr=%s", code, stderr.String())
	}
	if gotInv == nil || gotInv.String("spec", "") != wantSpec {
		t.Fatalf("主控应当收到内联 spec=%q，实际=%+v", wantSpec, gotInv)
	}
	if _, hasFile := gotInv.Flags["file"]; hasFile {
		t.Fatalf("请求不应携带 file 键：%+v", gotInv.Flags)
	}

	// 2. --file - 从 Stdin 读取并内联。
	gotInv = nil
	opts.Stdin = strings.NewReader(`{"op":"delete","kind":"Server","id":1,"resourceVersion":2}`)
	stdout.Reset()
	stderr.Reset()
	code = Execute(opts, []string{"plan", "--file", "-", "--json"}, &stdout, &stderr)
	if code != v1.ExitOK {
		t.Fatalf("plan --file - 失败 code=%d stderr=%s", code, stderr.String())
	}
	if gotInv == nil || !strings.Contains(gotInv.String("spec", ""), `"delete"`) {
		t.Fatalf("stdin 内联失败：%+v", gotInv)
	}

	// 3. --file 与 --spec 同时提供：usage 退出码 2，不发主控。
	gotInv = nil
	stdout.Reset()
	stderr.Reset()
	code = Execute(opts, []string{"plan", "--file", specPath, "--spec", "{}", "--json"}, &stdout, &stderr)
	if code != v1.ExitUsage || gotInv != nil {
		t.Fatalf("同时给 --file 和 --spec 应当退出码 2 且不发请求：code=%d gotInv=%v stderr=%s", code, gotInv, stderr.String())
	}

	// 4. 超过 4 MiB 上限：usage 退出码 2。
	bigPath := filepath.Join(dir, "big.json")
	if err := os.WriteFile(bigPath, bytes.Repeat([]byte("a"), inlineFileMaxBytes+10), 0o600); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	stderr.Reset()
	code = Execute(opts, []string{"plan", "--file", bigPath, "--json"}, &stdout, &stderr)
	if code != v1.ExitUsage {
		t.Fatalf("超限文件应当退出码 2：code=%d stderr=%s", code, stderr.String())
	}

	// 5. DynamicDanger 的 apply 接收 --confirm 与 --force 并经 REST 传到主控。
	gotInv = nil
	stdout.Reset()
	stderr.Reset()
	code = Execute(opts, []string{"apply", "--plan", "7", "--force", "--confirm", "hk-1", "--json"}, &stdout, &stderr)
	if code != v1.ExitOK {
		t.Fatalf("apply 失败 code=%d stderr=%s", code, stderr.String())
	}
	if gotInv == nil || gotInv.Confirm != "hk-1" || !gotInv.Bool("force") || gotInv.Int("plan", 0) != 7 {
		t.Fatalf("apply 参数解析不对：%+v", gotInv)
	}

	// 6. partial_failure 映射为退出码 8。
	partialOpts := Options{Table: tbl, Remote: func(string) command.Runner {
		return command.RunnerFunc(func(context.Context, *command.Invocation) (any, error) {
			return nil, v1.New(v1.CodePartialFailure, "部分节点失败").WithState("plan_id", 7)
		})
	}}
	stdout.Reset()
	stderr.Reset()
	code = Execute(partialOpts, []string{"apply", "--plan", "7", "--json"}, &stdout, &stderr)
	if code != v1.ExitPartialFailure {
		t.Fatalf("partial_failure 退出码应当为 %d，得到 %d stderr=%s", v1.ExitPartialFailure, code, stderr.String())
	}

	// 7. 直接向 REST 发 {"file":"/etc/passwd"} 必须被拒。
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/api/v1/plan", strings.NewReader(`{"file":"/etc/passwd"}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("REST 应拒收 file 参数，得到 %d", resp.StatusCode)
	}
	var apiErr v1.Error
	if err := json.NewDecoder(resp.Body).Decode(&apiErr); err != nil || apiErr.Code != v1.CodeBadRequest {
		t.Fatalf("REST 错误码不对：%+v %v", apiErr, err)
	}
}
