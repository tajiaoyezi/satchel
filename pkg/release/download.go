package release

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

const (
	// MaxBinarySize 是一个二进制的上限：超过就当这个源失败。
	MaxBinarySize = 256 << 20
	maxSigSize    = 1 << 10

	directTimeout = 2 * time.Minute
	proxyTimeout  = 5 * time.Minute
)

// Artifact 是一对验过签的二进制与分离签名，落在调用方指定的目录里。
type Artifact struct {
	Binary, Signature string
	Source            string // cdn、github 或 gh-proxy
}

// Remove 删掉这一对临时文件。
func (a *Artifact) Remove() {
	if a == nil {
		return
	}
	os.Remove(a.Binary)
	os.Remove(a.Signature)
}

// Progress 报告下载进度：source 是当前在用的源，total 不知道时为 0。
type Progress func(source string, bytes, total int64)

// BinaryName 是 Linux 上某个架构的发布物名（<project>-linux-<goarch>）。
func BinaryName(project, goarch string) string {
	if project == "" {
		project = "satchel"
	}
	return project + "-linux-" + goarch
}

// DownloadPair 按 CDN（useCDN 且 CDN 不空时）→ GitHub → gh-proxy 顺序，从同一个源成对下载 rel 的二进制与 .sig，
// 写到 dir 并调用 verify（为 nil 时默认用 VerifyFile）验签；任一样失败立刻删除本源产物整组换下一个源，绝不跨源混配。
func DownloadPair(ctx context.Context, src Sources, rel *Release, useCDN bool, goarch, dir string, verify func(bin, sig string) error, progress Progress) (*Artifact, error) {
	if verify == nil {
		verify = VerifyFile
	}
	name := BinaryName(src.project(), goarch)
	github := strings.TrimRight(src.Web, "/") + "/" + src.Repo + "/releases/download/" + rel.Tag + "/" + name
	type candidate struct {
		source, url string
		timeout     time.Duration
	}
	var cands []candidate
	if useCDN && src.CDN != "" {
		cands = append(cands, candidate{SourceCDN, strings.TrimRight(src.CDN, "/") + "/" + src.project() + "/releases/" + rel.Version + "/" + name, directTimeout})
	}
	cands = append(cands, candidate{SourceGitHub, github, directTimeout})
	if src.Proxy != "" {
		cands = append(cands, candidate{SourceProxy, src.Proxy + github, proxyTimeout})
	}
	var reasons []string
	for _, c := range cands {
		a, err := fetchPair(ctx, src, c.source, c.url, c.timeout, dir, verify, progress)
		if err == nil {
			return a, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		reasons = append(reasons, c.source+"："+err.Error())
	}
	return nil, errors.New(strings.Join(reasons, "；"))
}

func fetchPair(ctx context.Context, src Sources, source, url string, timeout time.Duration, dir string, verify func(bin, sig string) error, progress Progress) (*Artifact, error) {
	a := &Artifact{Source: source}
	var err error
	a.Binary, err = fetch(ctx, src, url, timeout, dir, MaxBinarySize, func(n, total int64) {
		if progress != nil {
			progress(source, n, total)
		}
	})
	if err == nil {
		a.Signature, err = fetch(ctx, src, url+".sig", timeout, dir, maxSigSize, nil)
	}
	if err == nil {
		if verr := verify(a.Binary, a.Signature); verr != nil {
			err = fmt.Errorf("验签不过：%w", verr)
		}
	}
	if err != nil {
		a.Remove()
		return nil, err
	}
	return a, nil
}

func fetch(ctx context.Context, src Sources, url string, timeout time.Duration, dir string, limit int64, onProgress func(n, total int64)) (path string, err error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "satchel-updater")
	resp, err := src.client().Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%s 返回 HTTP %d", url, resp.StatusCode)
	}
	if resp.ContentLength > limit {
		return "", fmt.Errorf("%s 有 %d 字节，超过上限 %d", url, resp.ContentLength, limit)
	}
	f, err := os.CreateTemp(dir, ".satchel-update-*")
	if err != nil {
		return "", err
	}
	defer func() {
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			os.Remove(f.Name())
		}
	}()
	w := &countingWriter{w: f, total: resp.ContentLength, onProgress: onProgress}
	n, err := io.Copy(w, io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return "", err
	}
	if n > limit {
		return "", fmt.Errorf("%s 超过上限 %d 字节", url, limit)
	}
	if err := f.Sync(); err != nil {
		return "", err
	}
	return f.Name(), nil
}

type countingWriter struct {
	w          io.Writer
	n, total   int64
	last       int64
	onProgress func(n, total int64)
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	if c.onProgress != nil && (c.n-c.last >= 1<<20 || c.n == c.total) {
		c.last = c.n
		c.onProgress(c.n, max(c.total, 0))
	}
	return n, err
}
