package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/satchel/satchel/internal/command"
	v1 "github.com/satchel/satchel/pkg/api/v1"
)

func shapeTable(t *testing.T) *command.Table {
	t.Helper()
	tbl, err := command.New(
		&command.Command{Path: []string{"job", "get"}, Summary: "s", Class: command.ClassRead, Args: []command.Arg{{Name: "job"}}},
		&command.Command{Path: []string{"backup", "create"}, Summary: "s", Class: command.ClassAction, Shape: command.ShapeJob},
		&command.Command{Path: []string{"backup", "upload"}, Summary: "s", Class: command.ClassAction, Shape: command.ShapeUpload,
			Flags: []command.Flag{{Name: command.UploadFlag, Type: command.TypeFile}, {Name: "note", Type: command.TypeString}}},
		&command.Command{Path: []string{"backup", "download"}, Summary: "s", Class: command.ClassAction, Shape: command.ShapeDownload,
			Args: []command.Arg{{Name: "name"}}, Flags: []command.Flag{{Name: command.DownloadFlag, Type: command.TypeFile}}},
	)
	if err != nil {
		t.Fatal(err)
	}
	return tbl
}

// jobRunner 是进程内的假执行链：backup create 受理一个 job，job get 第 finishAfter 次起返回结局。
type jobRunner struct {
	mu          sync.Mutex
	gets        int
	finishAfter int
	fail        bool
	uploaded    string
	uploadFlags map[string]any
}

func (r *jobRunner) Run(_ context.Context, inv *command.Invocation) (any, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	switch inv.Name() {
	case "backup create":
		return map[string]any{"job_id": "job-0123456789abcdef", "status": "queued"}, nil
	case "job get":
		r.gets++
		if r.gets < r.finishAfter {
			return map[string]any{"job_id": inv.Args[0], "status": "running"}, nil
		}
		if r.fail {
			out, _ := json.Marshal(v1.New(v1.CodeUnavailable, "找不到 pg_dump"))
			return map[string]any{"job_id": inv.Args[0], "status": "failed", "output": string(out)}, nil
		}
		return map[string]any{"job_id": inv.Args[0], "status": "done", "output": `{"name":"b.zip"}`}, nil
	case "backup upload":
		b, _ := io.ReadAll(inv.Body)
		r.uploaded, r.uploadFlags = string(b), inv.Flags
		return map[string]any{"name": "uploaded.zip"}, nil
	case "backup download":
		return &command.File{Name: "b.zip", ContentType: "application/zip", Open: func() (io.ReadCloser, error) {
			return io.NopCloser(strings.NewReader("zip-bytes")), nil
		}}, nil
	}
	return nil, v1.Newf(v1.CodeNotFound, "没有 %s", inv.Name())
}

func shapeOptions(t *testing.T, r command.Runner, maxWait time.Duration) Options {
	opts := Options{Table: shapeTable(t), Local: command.Bindings{}, Renderers: map[string]Renderer{},
		Remote: func(string) command.Runner { return r }, JobPoll: time.Millisecond, JobMaxWait: maxWait}
	return opts
}

func runShape(opts Options, args ...string) (string, string, int) {
	var out, errOut bytes.Buffer
	code := Execute(opts, args, &out, &errOut)
	return out.String(), errOut.String(), code
}

// master-jobs「三个投影怎么等 job」：CLI 跟到结束；--no-wait 不等；失败时给出 output 里的错误与退出码；限时到了返回当时的 job。
func TestFollowJob(t *testing.T) {
	r := &jobRunner{finishAfter: 3}
	out, errOut, code := runShape(shapeOptions(t, r, 0), "backup", "create", "--json")
	if code != 0 || !strings.Contains(out, `"status": "done"`) && !strings.Contains(out, `"status":"done"`) || r.gets != 3 {
		t.Fatalf("应当跟到 done：code=%d gets=%d out=%s err=%s", code, r.gets, out, errOut)
	}
	r = &jobRunner{finishAfter: 1}
	out, _, code = runShape(shapeOptions(t, r, 0), "backup", "create", "--no-wait", "--json")
	if code != 0 || !strings.Contains(out, "queued") || r.gets != 0 {
		t.Fatalf("--no-wait 不应当查：code=%d gets=%d out=%s", code, r.gets, out)
	}
	r = &jobRunner{finishAfter: 1, fail: true}
	_, errOut, code = runShape(shapeOptions(t, r, 0), "backup", "create", "--json")
	if code != 1 || !strings.Contains(errOut, "unavailable") {
		t.Fatalf("失败的 job 应当给出 unavailable 与退出码 1：code=%d err=%s", code, errOut)
	}
	r = &jobRunner{finishAfter: 1 << 30}
	out, _, code = runShape(shapeOptions(t, r, 20*time.Millisecond), "backup", "create", "--json")
	if code != 0 || !strings.Contains(out, "running") {
		t.Fatalf("到了最多等的时限应当返回当时的 job：code=%d out=%s", code, out)
	}
}

// 上传：--file 的内容作为 Body，路径本身不进 Flags；下载：写到 --output（0600），已存在就是 usage、不覆盖。
func TestUploadAndDownload(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "b.zip")
	os.WriteFile(src, []byte("PK-local"), 0o600)
	r := &jobRunner{}
	if _, errOut, code := runShape(shapeOptions(t, r, 0), "backup", "upload", "--file", src, "--note", "hi"); code != 0 {
		t.Fatalf("上传：%d %s", code, errOut)
	}
	if r.uploaded != "PK-local" || r.uploadFlags["note"] != "hi" || r.uploadFlags[command.UploadFlag] != nil {
		t.Fatalf("上传的请求体与 flag 不对：%q %v", r.uploaded, r.uploadFlags)
	}
	if _, _, code := runShape(shapeOptions(t, r, 0), "backup", "upload", "--file", filepath.Join(dir, "none.zip")); code != 2 {
		t.Fatalf("打不开的文件应当是 usage（退出码 2），得到 %d", code)
	}
	dst := filepath.Join(dir, "out.zip")
	if _, errOut, code := runShape(shapeOptions(t, r, 0), "backup", "download", "b.zip", "--output", dst); code != 0 {
		t.Fatalf("下载：%d %s", code, errOut)
	}
	if b, _ := os.ReadFile(dst); string(b) != "zip-bytes" {
		t.Fatalf("下载的内容：%q", b)
	}
	if info, _ := os.Stat(dst); info.Mode().Perm() != 0o600 {
		t.Fatalf("下载的文件应当 0600，得到 %o", info.Mode().Perm())
	}
	if _, _, code := runShape(shapeOptions(t, r, 0), "backup", "download", "b.zip", "--output", dst); code != 2 {
		t.Fatalf("已存在的 --output 应当是 usage，得到 %d", code)
	}
}

// 客户端：上传类命令的请求体是文件本身、flag 走查询参数；下载类命令的 200 响应是文件字节。
func TestClientUploadDownload(t *testing.T) {
	sock := filepath.Join(shortTempDir(t), "s.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	var gotBody, gotQuery, gotType string
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/download/b.zip") {
			w.Header().Set("Content-Type", "application/zip")
			w.Header().Set("Content-Disposition", `attachment; filename="b.zip"`)
			_, _ = w.Write([]byte("zip-bytes"))
			return
		}
		b, _ := io.ReadAll(r.Body)
		gotBody, gotQuery, gotType = string(b), r.URL.RawQuery, r.Header.Get("Content-Type")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"name":"uploaded.zip"}`))
	})}
	go srv.Serve(ln)
	defer srv.Close()
	client := NewClient(shapeTable(t), Connection{Socket: sock})
	ctx := context.Background()
	if _, err := client.Run(ctx, &command.Invocation{Path: []string{"backup", "upload"}, Flags: map[string]any{"note": "hi"}, Body: strings.NewReader("PK-body")}); err != nil {
		t.Fatal(err)
	}
	if gotBody != "PK-body" || gotQuery != "note=hi" || gotType != "application/zip" {
		t.Fatalf("上传的请求形状：body=%q query=%q type=%q", gotBody, gotQuery, gotType)
	}
	res, err := client.Run(ctx, &command.Invocation{Path: []string{"backup", "download"}, Args: []string{"b.zip"}, Flags: map[string]any{}})
	if err != nil {
		t.Fatal(err)
	}
	f, ok := res.(*command.File)
	if !ok || f.Name != "b.zip" {
		t.Fatalf("下载应当返回文件：%#v", res)
	}
	rc, _ := f.Open()
	b, _ := io.ReadAll(rc)
	rc.Close()
	if string(b) != "zip-bytes" {
		t.Fatalf("下载的字节：%q", b)
	}
}
