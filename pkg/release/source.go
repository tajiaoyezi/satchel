package release

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

// 发布源默认地址；各二进制的发布源包级变量须为常量子句初始化，才能被测试构建时的 -ldflags -X 覆盖（不接受环境变量覆盖）。
const (
	DefaultCDNBase   = ""
	DefaultGitHubAPI = "https://api.github.com"
	DefaultGitHubWeb = "https://github.com"
	DefaultGHProxy   = "https://gh-proxy.com/"
)

// Sources 是一次版本查询或下载使用的发布源配置。Project 决定 CDN 路径前缀与发布包前缀（如 satchel、satchel-agent）。
type Sources struct {
	CDN, Project, Repo, API, Web, Proxy string
	Client                              *http.Client
}

// DefaultSources 返回指定项目与 GitHub 仓库的默认发布源配置。
func DefaultSources(project, repo string) Sources {
	return Sources{
		CDN:     DefaultCDNBase,
		Project: project,
		Repo:    repo,
		API:     DefaultGitHubAPI,
		Web:     DefaultGitHubWeb,
		Proxy:   DefaultGHProxy,
	}
}

func (s Sources) project() string {
	if s.Project != "" {
		return s.Project
	}
	return "satchel"
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

// Release 是所选渠道的最新版本信息（可选含节点控制协议兼容性元数据）。
type Release struct {
	Version          string // 不带 v
	Tag              string // 发布的 tag，如 v0.1.1
	Notes            string
	URL              string // 发布页
	Prerelease       bool
	Source           string // 版本信息来源：cdn、github 或 gh-proxy
	ControlProtocol  int    `json:"control_protocol,omitempty"`
	MinMasterVersion string `json:"min_master_version,omitempty"`
}

// CDNStatus 说明这一次有没有用 CDN 及未用的原因。
type CDNStatus struct {
	Enabled bool   `json:"enabled"`
	Used    bool   `json:"used"`
	Reason  string `json:"reason,omitempty"`
}

// Latest 按 CDN → GitHub → gh-proxy 顺序取 channel 的最新版本。
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
	rel, err := latestGitHub(ctx, src, src.API, channel, SourceGitHub)
	if err == nil {
		return rel, cdn, nil
	}
	reasons = append(reasons, "GitHub："+err.Error())
	if src.Proxy != "" {
		rel, err := latestGitHub(ctx, src, src.Proxy+src.API, channel, SourceProxy)
		if err == nil {
			return rel, cdn, nil
		}
		reasons = append(reasons, "gh-proxy："+err.Error())
	}
	return nil, cdn, errors.New(strings.Join(reasons, "；"))
}

// cdnMeta 是 CDN 上 <project>/channels/<渠道>/version.json 的格式。
type cdnMeta struct {
	Version          string `json:"version"`
	Tag              string `json:"tag"`
	Notes            string `json:"notes"`
	HTMLURL          string `json:"html_url"`
	Prerelease       bool   `json:"prerelease"`
	ControlProtocol  int    `json:"control_protocol,omitempty"`
	MinMasterVersion string `json:"min_master_version,omitempty"`
}

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
	indexURL := strings.TrimRight(src.CDN, "/") + "/" + src.project() + "/channels/" + channel + "/version.json"
	if err := getJSON(ctx, src, cdnTimeout, indexURL, &meta); err != nil {
		return nil, err
	}
	if !Valid(meta.Version) {
		return nil, fmt.Errorf("version.json 的 version %q 不是合法的版本号", meta.Version)
	}
	if channel == ChannelStable && (meta.Prerelease || IsPrerelease(meta.Version)) {
		return nil, fmt.Errorf("stable 渠道的 version.json 指向预发布版本 %s", meta.Version)
	}
	tag := meta.Tag
	if tag == "" {
		tag = "v" + Normalize(meta.Version)
	}
	return &Release{
		Version:          Normalize(meta.Version),
		Tag:              tag,
		Notes:            meta.Notes,
		URL:              meta.HTMLURL,
		Prerelease:       meta.Prerelease,
		Source:           SourceCDN,
		ControlProtocol:  meta.ControlProtocol,
		MinMasterVersion: strings.TrimSpace(meta.MinMasterVersion),
	}, nil
}

type ghRelease struct {
	TagName    string `json:"tag_name"`
	HTMLURL    string `json:"html_url"`
	Body       string `json:"body"`
	Draft      bool   `json:"draft"`
	Prerelease bool   `json:"prerelease"`
}

func latestGitHub(ctx context.Context, src Sources, api, channel, source string) (*Release, error) {
	base := api + "/repos/" + src.Repo + "/releases"
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
	ctrlProto, minMaster := parseReleaseMeta(best.Body)
	return &Release{
		Version:          Normalize(best.TagName),
		Tag:              best.TagName,
		Notes:            best.Body,
		URL:              best.HTMLURL,
		Prerelease:       best.Prerelease,
		Source:           source,
		ControlProtocol:  ctrlProto,
		MinMasterVersion: minMaster,
	}, nil
}

func parseReleaseMeta(body string) (int, string) {
	const prefix = "<!-- satchel-meta:"
	_, rest, ok := strings.Cut(body, prefix)
	if !ok {
		return 0, ""
	}
	raw, _, ok := strings.Cut(rest, "-->")
	if !ok {
		return 0, ""
	}
	var m struct {
		ControlProtocol  int    `json:"control_protocol"`
		MinMasterVersion string `json:"min_master_version"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(raw)), &m); err != nil {
		return 0, ""
	}
	return m.ControlProtocol, strings.TrimSpace(m.MinMasterVersion)
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
