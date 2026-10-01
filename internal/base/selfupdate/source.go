package selfupdate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// 发布源的地址（master-self-update「版本信息从哪里来」）。是变量而不是常量：端到端测试构建二进制时用 -ldflags -X 换成
// 测试服务器，产品构建不改。不提供环境变量覆盖：能改更新源就能让主控装任意程序。
var (
	// CDNBase 是更新 CDN 的地址（无尾斜杠）。域名给出之前为空：开关开着也不走 CDN（第 09 章）。
	CDNBase = ""
	// GitHubRepo、GitHubAPI、GitHubWeb 是发布所在的 GitHub 仓库与接口、下载地址；与 install.sh 的 GITHUB_REPO 相同。
	GitHubRepo = "tajiaoyezi/satchel"
	GitHubAPI  = "https://api.github.com"
	GitHubWeb  = "https://github.com"
	// GHProxy 是直连 GitHub 失败时加在 GitHub 地址前面的代理前缀；与 install.sh 的 GH_PROXY 相同。只代理 GitHub，不代理 CDN。
	GHProxy = "https://gh-proxy.com/"
)

// Sources 是一次检查或下载用的发布源地址；产品里取 DefaultSources，测试换成 httptest 的地址。
type Sources struct {
	CDN, Repo, API, Web, Proxy string
	// Client 为空时用 http.DefaultClient；超时由各请求自己的 ctx 定。
	Client *http.Client
}

// DefaultSources 返回包级变量给的地址。
func DefaultSources() Sources {
	return Sources{CDN: CDNBase, Repo: GitHubRepo, API: GitHubAPI, Web: GitHubWeb, Proxy: GHProxy}
}

const (
	ChannelStable     = "stable"
	ChannelPrerelease = "prerelease"

	SourceCDN    = "cdn"
	SourceGitHub = "github"
	SourceProxy  = "gh-proxy"

	cdnTimeout    = 8 * time.Second
	githubTimeout = 30 * time.Second
)

// Release 是所选渠道的最新版本。
type Release struct {
	Version    string // 不带 v
	Tag        string // 发布的 tag，如 v0.1.1
	Notes      string
	URL        string // 发布页
	Prerelease bool
	Source     string // 版本信息从哪里来：cdn 或 github
}

// CDNStatus 说明这一次有没有用 CDN、没用的原因。
type CDNStatus struct {
	Enabled bool   `json:"enabled"`
	Used    bool   `json:"used"`
	Reason  string `json:"reason,omitempty"`
}

// Latest 按 CDN → GitHub 的顺序取 channel 的最新版本（master-self-update「版本信息从哪里来」）。cdnEnabled 是系统设置
// update_cdn_enabled 的值。两处都取不到时返回的错误带各处的原因。
func Latest(ctx context.Context, src Sources, channel string, cdnEnabled bool) (*Release, CDNStatus, error) {
	cdn := CDNStatus{Enabled: cdnEnabled}
	var reasons []string
	switch {
	case !cdnEnabled:
		cdn.Reason = "更新 CDN 已关闭"
	case src.CDN == "":
		cdn.Reason = "更新 CDN 的域名还没配置"
	default:
		rel, err := latestCDN(ctx, src, channel)
		if err == nil {
			cdn.Used = true
			return rel, cdn, nil
		}
		cdn.Reason = "更新 CDN 不可用：" + err.Error()
		reasons = append(reasons, "CDN："+err.Error())
	}
	rel, err := latestGitHub(ctx, src, channel)
	if err != nil {
		reasons = append(reasons, "GitHub："+err.Error())
		return nil, cdn, errors.New(strings.Join(reasons, "；"))
	}
	return rel, cdn, nil
}

// cdnMeta 是 CDN 上 satchel/channels/<渠道>/version.json 的格式（由发布线用 jq 生成）。
type cdnMeta struct {
	Version    string `json:"version"`
	Tag        string `json:"tag"`
	Notes      string `json:"notes"`
	HTMLURL    string `json:"html_url"`
	Prerelease bool   `json:"prerelease"`
}

// latestCDN 读 CDN 上渠道的版本索引。发布线只更新本次发布所在渠道的索引，所以 prerelease 渠道要同时读 stable 的索引、
// 取版本号大的那个（与 GitHub 上 prerelease 渠道也算正式版一致）；两个里有一个取得到就行。
func latestCDN(ctx context.Context, src Sources, channel string) (*Release, error) {
	if channel != ChannelPrerelease {
		return cdnIndex(ctx, src, channel)
	}
	pre, perr := cdnIndex(ctx, src, ChannelPrerelease)
	stable, serr := cdnIndex(ctx, src, ChannelStable)
	switch {
	case perr != nil && serr != nil:
		return nil, errors.Join(perr, serr)
	case perr != nil:
		return stable, nil
	case serr != nil || Compare(pre.Version, stable.Version) >= 0:
		return pre, nil
	}
	return stable, nil
}

func cdnIndex(ctx context.Context, src Sources, channel string) (*Release, error) {
	var meta cdnMeta
	if err := getJSON(ctx, src, cdnTimeout, src.CDN+"/satchel/channels/"+channel+"/version.json", &meta); err != nil {
		return nil, err
	}
	if !Valid(meta.Version) {
		return nil, fmt.Errorf("version.json 的 version %q 不是合法的版本号", meta.Version)
	}
	if channel == ChannelStable && (meta.Prerelease || len(parse(meta.Version).pre) > 0) {
		return nil, fmt.Errorf("stable 渠道的 version.json 指向预发布版本 %s", meta.Version)
	}
	tag := meta.Tag
	if tag == "" {
		tag = "v" + Normalize(meta.Version)
	}
	return &Release{Version: Normalize(meta.Version), Tag: tag, Notes: meta.Notes, URL: meta.HTMLURL, Prerelease: meta.Prerelease, Source: SourceCDN}, nil
}

type ghRelease struct {
	TagName    string `json:"tag_name"`
	HTMLURL    string `json:"html_url"`
	Body       string `json:"body"`
	Draft      bool   `json:"draft"`
	Prerelease bool   `json:"prerelease"`
}

// latestGitHub：stable 取 releases/latest；prerelease 取最近 30 个里不是草稿、版本号最大的一个（正式版也算）。
func latestGitHub(ctx context.Context, src Sources, channel string) (*Release, error) {
	base := src.API + "/repos/" + src.Repo + "/releases"
	var best *ghRelease
	if channel == ChannelPrerelease {
		var list []ghRelease
		if err := getJSON(ctx, src, githubTimeout, base+"?per_page=30", &list); err != nil {
			return nil, err
		}
		for i := range list {
			r := &list[i]
			if r.Draft || !Valid(r.TagName) {
				continue
			}
			if best == nil || Compare(r.TagName, best.TagName) > 0 {
				best = r
			}
		}
		if best == nil {
			return nil, errors.New("没有可用的发布")
		}
	} else {
		var r ghRelease
		if err := getJSON(ctx, src, githubTimeout, base+"/latest", &r); err != nil {
			return nil, err
		}
		if !Valid(r.TagName) {
			return nil, fmt.Errorf("最新发布的 tag %q 不是合法的版本号", r.TagName)
		}
		best = &r
	}
	return &Release{Version: Normalize(best.TagName), Tag: best.TagName, Notes: best.Body, URL: best.HTMLURL, Prerelease: best.Prerelease, Source: SourceGitHub}, nil
}

func getJSON(ctx context.Context, src Sources, timeout time.Duration, url string, out any) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "satchel-updater")
	req.Header.Set("Accept", "application/json")
	resp, err := src.client().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s 返回 HTTP %d", url, resp.StatusCode)
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(out); err != nil {
		return fmt.Errorf("解析 %s 失败：%w", url, err)
	}
	return nil
}

func (s Sources) client() *http.Client {
	if s.Client != nil {
		return s.Client
	}
	return http.DefaultClient
}
