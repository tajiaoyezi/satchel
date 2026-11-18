package selfupdate

import (
	"context"

	"github.com/satchel/satchel/pkg/release"
)

// MaxBinarySize 是一个二进制的上限：超过就当这个源失败。
const MaxBinarySize = release.MaxBinarySize

// Artifact 是一对验过签的二进制与签名，都在调用方给的目录里（以 . 开头的临时文件）。
type Artifact = release.Artifact

// Progress 报告下载进度：source 是当前在用的源，total 不知道时为 0。
type Progress = release.Progress

// BinaryName 是 Linux 上某个架构的发布物名（release-pipeline：satchel-<goos>-<goarch>）。
func BinaryName(goarch string) string { return release.BinaryName("satchel", goarch) }

// Download 按 CDN（useCDN 且地址不空时）→ GitHub → gh-proxy 的顺序，从同一个源成对取 rel 的二进制与 .sig，写到 dir 里，
// 用 verify 验签（master-self-update「版本信息从哪里来」）。
func Download(ctx context.Context, src Sources, rel *Release, useCDN bool, goarch, dir string, verify func(bin, sig string) error, progress Progress) (*Artifact, error) {
	if src.Project == "" {
		src.Project = "satchel"
	}
	return release.DownloadPair(ctx, src, rel, useCDN, goarch, dir, verify, progress)
}
