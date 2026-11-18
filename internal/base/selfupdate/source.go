package selfupdate

import (
	"context"

	"github.com/satchel/satchel/pkg/release"
)

// 发布源的地址（master-self-update「版本信息从哪里来」）。是变量而不是常量：端到端测试构建二进制时用 -ldflags -X 换成
// 测试服务器，产品构建不改。不提供环境变量覆盖：能改更新源就能让主控装任意程序。
var (
	// CDNBase 是更新 CDN 的地址（无尾斜杠）。域名给出之前为空：开关开着也不走 CDN（第 09 章）。
	CDNBase = release.DefaultCDNBase
	// GitHubRepo、GitHubAPI、GitHubWeb 是发布所在的 GitHub 仓库与接口、下载地址；与 install.sh 的 GITHUB_REPO 相同。
	GitHubRepo = "tajiaoyezi/satchel"
	GitHubAPI  = release.DefaultGitHubAPI
	GitHubWeb  = release.DefaultGitHubWeb
	// GHProxy 是直连 GitHub 失败时加在 GitHub 地址前面的代理前缀；与 install.sh 的 GH_PROXY 相同。只代理 GitHub，不代理 CDN。
	GHProxy = release.DefaultGHProxy
)

// Sources 是一次检查或下载用的发布源地址；产品里取 DefaultSources，测试换成 httptest 的地址。
type Sources = release.Sources

// DefaultSources 返回包级变量给的地址。
func DefaultSources() Sources {
	return Sources{CDN: CDNBase, Project: "satchel", Repo: GitHubRepo, API: GitHubAPI, Web: GitHubWeb, Proxy: GHProxy}
}

const (
	ChannelStable     = release.ChannelStable
	ChannelPrerelease = release.ChannelPrerelease

	SourceCDN    = release.SourceCDN
	SourceGitHub = release.SourceGitHub
	SourceProxy  = release.SourceProxy
)

// Release 是所选渠道的最新版本。
type Release = release.Release

// CDNStatus 说明这一次有没有用 CDN、没用的原因。
type CDNStatus = release.CDNStatus

// Latest 按 CDN → GitHub → gh-proxy 的顺序取 channel 的最新版本（master-self-update「版本信息从哪里来」）。
func Latest(ctx context.Context, src Sources, channel string, cdnEnabled bool) (*Release, CDNStatus, error) {
	if src.Project == "" {
		src.Project = "satchel"
	}
	return release.Latest(ctx, src, channel, cdnEnabled)
}
