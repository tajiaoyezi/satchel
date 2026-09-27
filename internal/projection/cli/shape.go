package cli

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/satchel/satchel/internal/command"
	v1 "github.com/satchel/satchel/pkg/api/v1"
)

// openUpload 打开上传类命令的本地文件（--file），把它作为请求体放进 inv.Body，并从 Flags 里拿掉 --file（路径不发给主控）。
// 调用方负责关闭返回的文件。
func openUpload(inv *command.Invocation) (*os.File, error) {
	path, _ := inv.Flags[command.UploadFlag].(string)
	delete(inv.Flags, command.UploadFlag)
	if path == "" {
		return nil, usageError("要用 --%s 指定本地文件", command.UploadFlag)
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, usageError("打不开 --%s 指定的文件 %s：%v", command.UploadFlag, path, err)
	}
	inv.Body = f
	return f, nil
}

// checkOutput 检查下载类命令的 --output：必填、不能已存在（不覆盖本地文件），并从 Flags 里拿掉它（路径不发给主控）。
func checkOutput(inv *command.Invocation) (string, error) {
	path, _ := inv.Flags[command.DownloadFlag].(string)
	delete(inv.Flags, command.DownloadFlag)
	if path == "" {
		return "", usageError("要用 --%s 指定写到本地的路径", command.DownloadFlag)
	}
	if _, err := os.Lstat(path); err == nil {
		return "", usageError("--%s 指定的 %s 已存在，不覆盖", command.DownloadFlag, path)
	}
	return path, nil
}

// saveDownload 把下载类命令的结果写到本地：先写同目录的临时文件（0600），写完再改名；目标在这期间出现了也不覆盖。
func saveDownload(result any, path string) (any, error) {
	f, ok := result.(*command.File)
	if !ok {
		return nil, v1.New(v1.CodeInternal, "主控没有返回文件")
	}
	src, err := f.Open()
	if err != nil {
		return nil, err
	}
	defer src.Close()
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return nil, v1.Wrap(v1.CodeInternal, "在本地建临时文件失败", err)
	}
	defer os.Remove(tmp.Name())
	n, err := io.Copy(tmp, src)
	if err == nil {
		err = tmp.Chmod(0o600)
	}
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return nil, v1.Wrap(v1.CodeUnavailable, "下载中断", err)
	}
	// 用硬链接落到目标名：目标已存在时失败而不是覆盖。
	if err := os.Link(tmp.Name(), path); err != nil {
		if errors.Is(err, os.ErrExist) {
			return nil, usageError("--%s 指定的 %s 已存在，不覆盖", command.DownloadFlag, path)
		}
		return nil, v1.Wrap(v1.CodeInternal, "写入 "+path+" 失败", err)
	}
	return map[string]any{"name": f.Name, "size": n, "output": path}, nil
}

// followJob 跟一个长任务到结束（master-jobs「三个投影怎么等 job」）：每隔 JobPoll 调一次 job get；done 时返回最终的 job，
// failed 时返回 output 里的四字段错误（CLI 按它给退出码）。JobMaxWait 非 0 时到点就返回当时的 job（MCP 最多等 60 秒），
// 这时即使失败也返回 job 本身，不当成错误。
func followJob(ctx context.Context, accepted any, runner command.Runner, opts Options) (any, error) {
	job, err := asJob(accepted)
	if err != nil {
		return nil, err
	}
	var deadline time.Time
	if opts.JobMaxWait > 0 {
		deadline = time.Now().Add(opts.JobMaxWait)
	}
	poll := opts.JobPoll
	if poll <= 0 {
		poll = time.Second
	}
	for !finished(job) {
		if !deadline.IsZero() && time.Now().After(deadline) {
			return job, nil
		}
		select {
		case <-ctx.Done():
			return nil, v1.Wrap(v1.CodeUnavailable, "等长任务时被中断", ctx.Err()).WithNext("用 satchel job get " + str(job["job_id"]) + " 接着查")
		case <-time.After(poll):
		}
		got, err := runner.Run(ctx, &command.Invocation{Path: []string{"job", "get"}, Args: []string{str(job["job_id"])}, Flags: map[string]any{}})
		if err != nil {
			return nil, err
		}
		if job, err = asJob(got); err != nil {
			return nil, err
		}
	}
	if job["status"] == "failed" && opts.JobMaxWait == 0 {
		var e v1.Error
		if err := json.Unmarshal([]byte(str(job["output"])), &e); err == nil && e.Code != "" {
			return nil, &e
		}
		return nil, v1.Newf(v1.CodeInternal, "长任务 %s 失败", str(job["job_id"]))
	}
	return job, nil
}

// asJob 把执行器返回的 job（客户端是 map，进程内的执行链是结构体）统一成 map。
func asJob(v any) (map[string]any, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, v1.Wrap(v1.CodeInternal, "编码 job 失败", err)
	}
	var job map[string]any
	if err := json.Unmarshal(raw, &job); err != nil || str(job["job_id"]) == "" {
		return nil, v1.New(v1.CodeInternal, "主控返回的不是一个 job")
	}
	return job, nil
}

func finished(job map[string]any) bool {
	switch job["status"] {
	case "done", "failed", "unknown":
		return true
	}
	return false
}

func str(v any) string {
	s, _ := v.(string)
	return s
}
