package rest

import (
	"bytes"
	"context"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/satchel/satchel/internal/command"
	v1 "github.com/satchel/satchel/pkg/api/v1"
)

// shapeRunner 按命令名给出三种执行形状的结果：上传读出请求体、下载返回文件、长任务返回一个 job 对象。
type shapeRunner struct {
	body    string
	last    *command.Invocation
	openErr error
}

func (s *shapeRunner) Run(_ context.Context, inv *command.Invocation) (any, error) {
	s.last = inv
	switch inv.Name() {
	case "backup upload":
		b, _ := io.ReadAll(inv.Body)
		s.body = string(b)
		return map[string]any{"name": "uploaded.zip", "size": len(b)}, nil
	case "backup download":
		return &command.File{Name: "b.zip", ContentType: "application/zip", Size: 5, Open: func() (io.ReadCloser, error) {
			if s.openErr != nil {
				return nil, s.openErr
			}
			return io.NopCloser(strings.NewReader("PK\x03\x04x")), nil
		}}, nil
	}
	return map[string]any{"job_id": "job-0123456789abcdef", "status": "queued"}, nil
}

func shapeTable(t *testing.T) *command.Table {
	t.Helper()
	tbl, err := command.New(
		&command.Command{Path: []string{"backup", "upload"}, Summary: "s", Class: command.ClassAction, Shape: command.ShapeUpload,
			Flags: []command.Flag{{Name: command.UploadFlag, Type: command.TypeFile}, {Name: "note", Type: command.TypeString}}},
		&command.Command{Path: []string{"backup", "download"}, Summary: "s", Class: command.ClassAction, Shape: command.ShapeDownload,
			Args: []command.Arg{{Name: "name"}}, Flags: []command.Flag{{Name: command.DownloadFlag, Type: command.TypeFile}}},
		&command.Command{Path: []string{"backup", "create"}, Summary: "s", Class: command.ClassAction, Shape: command.ShapeJob},
	)
	if err != nil {
		t.Fatal(err)
	}
	return tbl
}

// master-rest-api「请求解码」：上传类命令的请求体是文件本身，flag 走查询参数。
func TestUploadBody(t *testing.T) {
	r := &shapeRunner{}
	h := NewHandler(shapeTable(t), r, nil)
	code, fields := do(t, h, "POST", "/api/v1/backup/upload?note=hi", "PK\x03\x04zip-bytes", map[string]string{"Content-Type": "application/zip"})
	if code != 200 || r.body != "PK\x03\x04zip-bytes" || r.last.Flags["note"] != "hi" || !strings.Contains(string(fields["name"]), "uploaded.zip") {
		t.Fatalf("上传：%d %v body=%q flags=%v", code, fields, r.body, r.last.Flags)
	}
	for _, c := range []struct{ path, ctype string }{
		{"/api/v1/backup/upload", "application/json"},
		{"/api/v1/backup/upload?bogus=1", "application/zip"},
		{"/api/v1/backup/upload?file=/etc/passwd", "application/zip"},
	} {
		r.last = nil
		if code, fields := do(t, h, "POST", c.path, "x", map[string]string{"Content-Type": c.ctype}); code != 400 || errorOf(t, fields).Code != v1.CodeBadRequest || r.last != nil {
			t.Errorf("%s（%s）应当 400 且不到执行链：%d %v", c.path, c.ctype, code, fields)
		}
	}
}

// master-rest-api「成功响应」：下载类命令回文件字节，失败仍是四字段错误；长任务回 job 对象，--no-wait 在 REST 上无害。
func TestDownloadAndJob(t *testing.T) {
	r := &shapeRunner{}
	h := NewHandler(shapeTable(t), r, nil)
	req := httptest.NewRequest("POST", "/api/v1/backup/download/b.zip", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 || rec.Header().Get("Content-Type") != "application/zip" || !strings.Contains(rec.Header().Get("Content-Disposition"), `filename=b.zip`) ||
		!bytes.Equal(rec.Body.Bytes(), []byte("PK\x03\x04x")) || rec.Header().Get("Content-Length") != "5" {
		t.Fatalf("下载：%d %v %q", rec.Code, rec.Header(), rec.Body.String())
	}
	r.openErr = v1.New(v1.CodeNotFound, "没有这份备份")
	if code, fields := do(t, h, "POST", "/api/v1/backup/download/b.zip", `{}`, map[string]string{"Content-Type": "application/json"}); code != 404 || errorOf(t, fields).Code != v1.CodeNotFound {
		t.Fatalf("打不开时应当是四字段错误：%d %v", code, fields)
	}
	if code, fields := do(t, h, "POST", "/api/v1/backup/create", `{"no-wait":true}`, map[string]string{"Content-Type": "application/json"}); code != 200 || !strings.Contains(string(fields["job_id"]), "job-") {
		t.Fatalf("长任务：%d %v", code, fields)
	}
}
