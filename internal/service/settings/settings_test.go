package settings

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/uptrace/bun"

	"github.com/satchel/satchel/internal/base/db/dbtest"
	"github.com/satchel/satchel/internal/base/model"
	"github.com/satchel/satchel/internal/base/schema"
	"github.com/satchel/satchel/internal/base/store"
	"github.com/satchel/satchel/internal/command"
	core "github.com/satchel/satchel/internal/core/settings"
	v1 "github.com/satchel/satchel/pkg/api/v1"
)

type fixture struct {
	t     *testing.T
	db    *bun.DB
	svc   *Service
	admin context.Context
	user  context.Context
}

func setup(t *testing.T, bdb *bun.DB) *fixture {
	t.Helper()
	repo := core.New(bdb, store.New(bdb, schema.Default()), schema.Default())
	if err := repo.EnsureSingleton(context.Background()); err != nil {
		t.Fatal(err)
	}
	return &fixture{
		t: t, db: bdb, svc: New(repo),
		admin: v1.WithIdentity(context.Background(), v1.LocalAdmin("root")),
		user:  v1.WithIdentity(context.Background(), v1.Identity{Actor: "bob", ActorKind: v1.ActorUser, Role: v1.RoleUser, Scopes: []v1.Scope{v1.ScopeRead, v1.ScopeOperate}, Danger: []v1.Danger{}}),
	}
}

func (f *fixture) run(ctx context.Context, name string, args []string, flags map[string]any) (any, error) {
	f.t.Helper()
	h, ok := f.svc.Bindings()[name]
	if !ok {
		f.t.Fatalf("没有命令 %s 的处理函数", name)
	}
	if flags == nil {
		flags = map[string]any{}
	}
	return h(ctx, &command.Invocation{Path: strings.Fields(name), Args: args, Flags: flags})
}

// set 以管理员身份执行 settings set；values 里的值按调用方给的类型原样传（CLI 是字符串，REST 是原生 JSON 类型）。
func (f *fixture) set(values map[string]any, version int, extra ...string) (*Object, error) {
	f.t.Helper()
	flags := map[string]any{"set": values, "resource-version": version}
	for _, e := range extra {
		if e == "force" {
			flags["force"] = true
		}
	}
	res, err := f.run(f.admin, "settings set", nil, flags)
	if err != nil {
		return nil, err
	}
	return res.(*Object), nil
}

func (f *fixture) mustSet(values map[string]any, version int) *Object {
	f.t.Helper()
	obj, err := f.set(values, version)
	if err != nil {
		f.t.Fatalf("settings set %v 失败：%v", values, err)
	}
	return obj
}

func (f *fixture) show() *Object {
	f.t.Helper()
	res, err := f.run(f.admin, "settings show", nil, nil)
	if err != nil {
		f.t.Fatal(err)
	}
	return res.(*Object)
}

func (f *fixture) snapshots() int {
	f.t.Helper()
	n, err := f.db.NewSelect().Model((*model.ConfigSnapshot)(nil)).Count(context.Background())
	if err != nil {
		f.t.Fatal(err)
	}
	return n
}

func wantCode(t *testing.T, err error, code v1.Code) *v1.Error {
	t.Helper()
	var e *v1.Error
	if !errors.As(err, &e) || e.Code != code {
		t.Fatalf("想要错误码 %s，得到 %v", code, err)
	}
	return e
}

// jsonFields 把输出按对外的样子（MarshalOutput）解开，看 spec / status 的键与打码。
func jsonFields(t *testing.T, result any) (spec, status map[string]json.RawMessage, all string) {
	t.Helper()
	raw, err := v1.MarshalOutput(result)
	if err != nil {
		t.Fatal(err)
	}
	var top struct {
		APIVersion string                     `json:"apiVersion"`
		Kind       string                     `json:"kind"`
		Metadata   map[string]json.RawMessage `json:"metadata"`
		Spec       map[string]json.RawMessage `json:"spec"`
		Status     map[string]json.RawMessage `json:"status"`
	}
	if err := json.Unmarshal(raw, &top); err != nil {
		t.Fatal(err)
	}
	if top.APIVersion != v1.APIVersion || top.Kind != "SystemSettings" {
		t.Fatalf("信封不对：%s", raw)
	}
	if _, ok := top.Metadata["name"]; ok {
		t.Error("没有自然键的 kind 不该有 metadata.name")
	}
	return top.Spec, top.Status, string(raw)
}

// master-settings「settings show 返回合并后的整个对象」「settings 命令只对管理员开放」。
func TestShow(t *testing.T) {
	dbtest.ForEach(t, func(t *testing.T, bdb *bun.DB) {
		f := setup(t, bdb)
		obj := f.show()
		if obj.Metadata.ID != 1 || obj.Metadata.ResourceVersion != 1 || obj.Metadata.CreatedAt.IsZero() {
			t.Fatalf("metadata 不对：%+v", obj.Metadata)
		}
		if obj.Spec.HeartbeatInterval != 30 || !obj.Spec.EnableShortLink || obj.Spec.DefaultTheme != "flat" || obj.Spec.DashboardRefreshIntervalMs != 5000 ||
			obj.Spec.ProbeDisguisePingIntervalMs != 60000 || !obj.Spec.ProbeDisguiseMetricTraffic || obj.Spec.ProbeDisguiseShowName || obj.Spec.BrandingSiteTitle != "" {
			t.Fatalf("spec 默认值不对：%+v", obj.Spec)
		}
		if !obj.Status.RequireEncryption || obj.Status.MasterURL != "" || !obj.Status.UpdateCdnEnabled || obj.Status.MasterHttpsRecoveryPending {
			t.Fatalf("status 默认值不对：%+v", obj.Status)
		}
		spec, status, _ := jsonFields(t, obj)
		if _, ok := spec["master_url"]; ok {
			t.Error("spec 里不该有 master_url")
		}
		if _, ok := status["heartbeat_interval"]; ok {
			t.Error("status 里不该有 heartbeat_interval")
		}
		if len(spec) != 100 || len(status) != 38 {
			t.Errorf("spec 应当 100 个字段、status 38 个，得到 %d / %d", len(spec), len(status))
		}
		if string(status["telegram_bot_token"]) != `""` || string(spec["probe_external_token_sha256"]) != `""` {
			t.Errorf("空的打码字段输出空串：%s %s", status["telegram_bot_token"], spec["probe_external_token_sha256"])
		}
		// 普通用户被拒。
		if _, err := f.run(f.user, "settings show", nil, nil); v1.AsError(err).Code != v1.CodeForbidden {
			t.Errorf("普通用户应当 forbidden：%v", err)
		}
		if _, err := f.run(f.user, "settings set", nil, map[string]any{"set": map[string]any{"heartbeat_interval": "45"}, "resource-version": 1}); v1.AsError(err).Code != v1.CodeForbidden {
			t.Errorf("普通用户应当 forbidden：%v", err)
		}
		if _, err := f.run(f.user, "settings snapshots list", nil, nil); v1.AsError(err).Code != v1.CodeForbidden {
			t.Errorf("普通用户应当 forbidden：%v", err)
		}
	})
}

// master-settings「settings set 是日常运维档的事务写」。
func TestSet(t *testing.T) {
	dbtest.ForEach(t, func(t *testing.T, bdb *bun.DB) {
		f := setup(t, bdb)
		// 列与 key 一起改，CLI 形式的字符串值。
		obj := f.mustSet(map[string]any{"heartbeat_interval": "45", "branding_site_title": "Satchel"}, 1)
		if obj.Metadata.ResourceVersion != 2 || obj.Spec.HeartbeatInterval != 45 || obj.Spec.BrandingSiteTitle != "Satchel" || f.snapshots() != 1 {
			t.Fatalf("写后：%+v %d", obj.Metadata, f.snapshots())
		}
		// REST 形式的原生类型（含 json.Number）。
		obj = f.mustSet(map[string]any{"heartbeat_interval": json.Number("50"), "agent_log_enabled": true, "probe_disguise_server_ids": []any{float64(1), json.Number("2")}}, 2)
		if obj.Spec.HeartbeatInterval != 50 || !obj.Spec.AgentLogEnabled || string(obj.Spec.ProbeDisguiseServerIds) != "[1,2]" {
			t.Fatalf("原生类型：%+v", obj.Spec)
		}
		// 布尔列写回 false 也落地（bun 只写点名的列，零值不会被跳过）。
		if obj := f.mustSet(map[string]any{"agent_log_enabled": "0"}, 3); obj.Spec.AgentLogEnabled || obj.Metadata.ResourceVersion != 4 {
			t.Fatalf("布尔列写 false：%+v", obj.Spec.AgentLogEnabled)
		}
		f.mustSet(map[string]any{"agent_log_enabled": "1"}, 4)
		if row := f.show(); !row.Spec.AgentLogEnabled || row.Metadata.ResourceVersion != 5 {
			t.Fatalf("布尔列写回 true：%+v", row.Metadata)
		}
		// 后面的期望版本都从 show 里取，不写死。
		version := int(f.show().Metadata.ResourceVersion)
		// 混进别的档整单拒绝。
		err := func() error {
			_, err := f.set(map[string]any{"heartbeat_interval": "60", "master_url": "https://a.example"}, version)
			return err
		}()
		e := wantCode(t, err, v1.CodeFieldNotApplyable)
		if !strings.Contains(e.Reason, "master_url") || !strings.Contains(e.Reason, "人类专属") {
			t.Errorf("reason 应当点名 master_url 与人类专属：%s", e.Reason)
		}
		for name, value := range map[string]any{"update_cdn_enabled": "false", "require_encryption": "false", "master_https_recovery_pending": "true"} {
			if _, err := f.set(map[string]any{name: value}, version); v1.AsError(err).Code != v1.CodeFieldNotApplyable {
				t.Errorf("%s 应当 field_not_applyable：%v", name, err)
			}
		}
		if got := f.show(); got.Spec.HeartbeatInterval != 50 || int(got.Metadata.ResourceVersion) != version || f.snapshots() != 4 {
			t.Fatalf("被拒的写不该改任何东西：%+v %d", got.Metadata, f.snapshots())
		}
		// 未知字段与类型错误。
		if _, err := f.set(map[string]any{"heartbeat": "45"}, version); v1.AsError(err).Code != v1.CodeUnknownField {
			t.Errorf("未知字段：%v", err)
		}
		for name, value := range map[string]any{"heartbeat_interval": "abc", "probe_disguise_server_ids": "not json", "agent_log_enabled": "yes", "branding_site_title": float64(5), "probe_disguise_ping_targets": "null"} {
			_, err := f.set(map[string]any{name: value}, version)
			e := wantCode(t, err, v1.CodeBadRequest)
			if !strings.Contains(e.Reason, name) {
				t.Errorf("reason 应当点名 %s：%s", name, e.Reason)
			}
		}
		if _, err := f.set(map[string]any{"heartbeat_interval": "30 "}, version); v1.AsError(err).Code != v1.CodeBadRequest || !strings.Contains(v1.AsError(err).Reason, "30 ") {
			t.Errorf("带空格的数字应当拒绝并点名值：%v", err)
		}
		// 空 set 与缺版本。
		if _, err := f.set(map[string]any{}, version); v1.AsError(err).Code != v1.CodeBadRequest {
			t.Errorf("空 set：%v", err)
		}
		_, err = f.run(f.admin, "settings set", nil, map[string]any{"set": map[string]any{"heartbeat_interval": "45"}})
		e = wantCode(t, err, v1.CodeBadRequest)
		if !strings.Contains(e.Reason, "resource-version") || !strings.Contains(e.Next, "settings show") {
			t.Errorf("缺版本的提示：%s / %s", e.Reason, e.Next)
		}
		if _, err := f.run(f.admin, "settings set", nil, map[string]any{"set": map[string]any{"heartbeat_interval": "45"}, "resource-version": "1"}); v1.AsError(err).Code != v1.CodeBadRequest {
			t.Errorf("版本不是整数：%v", err)
		}
		// 过期版本与 force。
		e = wantCode(t, func() error { _, err := f.set(map[string]any{"heartbeat_interval": "60"}, 1); return err }(), v1.CodeVersionConflict)
		if e.State["resourceVersion"] != int64(version) || f.snapshots() != 4 {
			t.Errorf("version_conflict 的 state 与快照数：%v %d", e.State, f.snapshots())
		}
		obj, err = f.set(map[string]any{"heartbeat_interval": "60"}, 1, "force")
		if err != nil || int(obj.Metadata.ResourceVersion) != version+1 || obj.Spec.HeartbeatInterval != 60 || f.snapshots() != 5 {
			t.Fatalf("force：%v %+v %d", err, obj.Metadata, f.snapshots())
		}
		version++
		// 打码字段：写进去、读出来是 ***、*** 交回表示不变、空串清掉；形状是 64 位小写十六进制。
		token := strings.Repeat("ab", 32)
		if _, err := f.set(map[string]any{"probe_external_token_sha256": "abc123"}, version); v1.AsError(err).Code != v1.CodeBadRequest {
			t.Errorf("不是 SHA-256 形状的 token 应当拒绝：%v", err)
		}
		f.mustSet(map[string]any{"probe_external_token_sha256": token}, version)
		version++
		_, _, all := jsonFields(t, f.show())
		if strings.Contains(all, token) || !strings.Contains(all, `"probe_external_token_sha256":"***"`) {
			t.Errorf("打码字段应当输出 ***：%s", all)
		}
		f.mustSet(map[string]any{"probe_external_token_sha256": "***", "heartbeat_interval": "45"}, version)
		version++
		var entry model.SystemSettingEntry
		if err := bdb.NewSelect().Model(&entry).Where("key = ?", "probe_external_token_sha256").Scan(context.Background()); err != nil || entry.Value != token {
			t.Fatalf("*** 应当保持不变：%v %+v", err, entry)
		}
		if _, err := f.set(map[string]any{"probe_external_token_sha256": "***"}, version); v1.AsError(err).Code != v1.CodeBadRequest {
			t.Errorf("只有 *** 等于没有要改的字段：%v", err)
		}
		f.mustSet(map[string]any{"probe_external_token_sha256": ""}, version)
		if err := bdb.NewSelect().Model(&entry).Where("key = ?", "probe_external_token_sha256").Scan(context.Background()); err != nil || entry.Value != "" {
			t.Fatalf("空串应当清掉：%v %+v", err, entry)
		}
	})
}

// master-settings「字段规则照 mmwx 的写侧校验，只拒绝不改写」。
func TestRules(t *testing.T) {
	dbtest.ForEach(t, func(t *testing.T, bdb *bun.DB) {
		f := setup(t, bdb)
		bad := map[string]any{
			"subscription_output_format": "xml", "default_theme": "dark", "heartbeat_interval": "4", "traffic_collect_interval": "9",
			"traffic_check_interval": "9", "speed_collect_interval": "0", "dashboard_refresh_interval_ms": "500", "probe_disguise_ping_interval_ms": "1999",
			"probe_disguise_theme": "bad name!", "notify_traffic_threshold_percent": "0", "notify_package_expiring_days": "366",
			"proxy_groups_source_url": "ftp://x", "login_wallpaper": strings.Repeat("a", 2001), "probe_disguise_logo": "logo.png",
			"sub_rate_limit": "0", "user_quota_subscribe": "-1", "silent_mode_timeout": "0",
		}
		for name, value := range bad {
			_, err := f.set(map[string]any{name: value}, 1)
			if name == "silent_mode_timeout" {
				// 人类专属列：分档先于规则。
				wantCode(t, err, v1.CodeFieldNotApplyable)
				continue
			}
			e := wantCode(t, err, v1.CodeBadRequest)
			if !strings.Contains(e.Reason, name) {
				t.Errorf("reason 应当点名 %s：%s", name, e.Reason)
			}
		}
		obj := f.show()
		if obj.Metadata.ResourceVersion != 1 || obj.Spec.HeartbeatInterval != 30 || obj.Spec.DashboardRefreshIntervalMs != 5000 || obj.Spec.TrafficCollectInterval != 60 {
			t.Fatalf("被拒的值不该被改成默认值写进去：%+v", obj.Spec)
		}
		good := map[string]any{
			"subscription_output_format": "json", "default_theme": "premium", "heartbeat_interval": "5", "traffic_collect_interval": "10",
			"dashboard_refresh_interval_ms": "1000", "probe_disguise_ping_interval_ms": "300000", "probe_disguise_theme": "", "notify_traffic_threshold_percent": "100",
			"proxy_groups_source_url": "https://example.com/groups.yaml", "probe_disguise_logo": "data:image/png;base64,AAAA", "user_quota_subscribe": "0",
			"probe_disguise_ping_targets": `[{"host":"1.1.1.1","type":"icmp"}]`,
		}
		obj = f.mustSet(good, 1)
		if obj.Spec.DefaultTheme != "premium" || obj.Spec.SubscriptionOutputFormat != "json" || obj.Spec.HeartbeatInterval != 5 {
			t.Fatalf("边界值应当写进去：%+v", obj.Spec)
		}
	})
}

// master-settings「每次写都存一份写前快照」「回滚是把快照内容当一次 settings set 写回」。
func TestSnapshotsAndRollback(t *testing.T) {
	dbtest.ForEach(t, func(t *testing.T, bdb *bun.DB) {
		f := setup(t, bdb)
		f.mustSet(map[string]any{"heartbeat_interval": "45"}, 1)
		f.mustSet(map[string]any{"heartbeat_interval": "50", "branding_site_title": "S"}, 2)
		res, err := f.run(f.admin, "settings snapshots list", nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		page := res.(*command.PageResult)
		if page.Total != 2 || len(page.Items) != 2 || page.NextCursor != "" {
			t.Fatalf("列表：%+v", page)
		}
		first := page.Items[0].(core.SnapshotMeta)
		second := page.Items[1].(core.SnapshotMeta)
		if first.ObjectVersion != 2 || second.ObjectVersion != 1 || first.Source != core.SourceSettings || first.ContentHash == "" {
			t.Fatalf("倒序与元数据：%+v %+v", first, second)
		}
		raw, _ := json.Marshal(page)
		if strings.Contains(string(raw), `"content"`) {
			t.Error("列表不该带 content")
		}
		// 分页参数。
		res, err = f.run(f.admin, "settings snapshots list", nil, map[string]any{})
		if err != nil {
			t.Fatal(err)
		}
		paged := &command.Invocation{Path: []string{"settings", "snapshots", "list"}, Flags: map[string]any{}, Page: &command.Page{Limit: 1}}
		res, err = f.svc.Bindings()["settings snapshots list"](f.admin, paged)
		if err != nil || len(res.(*command.PageResult).Items) != 1 || res.(*command.PageResult).NextCursor == "" {
			t.Fatalf("limit 1 应当有下一页：%v %+v", err, res)
		}
		// 回滚到第一份快照（版本 1 的内容：heartbeat_interval 30、branding_site_title 空）。
		res, err = f.run(f.admin, "settings rollback", []string{jsonID(second.ID)}, map[string]any{"resource-version": 3})
		if err != nil {
			t.Fatal(err)
		}
		obj := res.(*Object)
		if obj.Metadata.ResourceVersion != 4 || obj.Spec.HeartbeatInterval != 30 || obj.Spec.BrandingSiteTitle != "" || f.snapshots() != 3 {
			t.Fatalf("回滚后：%+v %+v %d", obj.Metadata, obj.Spec, f.snapshots())
		}
		var newest model.ConfigSnapshot
		if err := bdb.NewSelect().Model(&newest).OrderExpr("id DESC").Limit(1).Scan(context.Background()); err != nil || newest.Source != core.SourceRollback || newest.ObjectVersion != 3 {
			t.Fatalf("回滚也留写前快照：%v %+v", err, newest)
		}
		var content map[string]any
		_ = json.Unmarshal(newest.Content, &content)
		if content["heartbeat_interval"] != float64(50) {
			t.Errorf("回滚前的快照内容应当是 50：%v", content["heartbeat_interval"])
		}
		// 缺版本、坏 id、不存在、别的 kind。
		if _, err := f.run(f.admin, "settings rollback", []string{jsonID(second.ID)}, nil); v1.AsError(err).Code != v1.CodeBadRequest {
			t.Errorf("缺版本：%v", err)
		}
		if _, err := f.run(f.admin, "settings rollback", []string{"abc"}, map[string]any{"resource-version": 4}); v1.AsError(err).Code != v1.CodeBadRequest {
			t.Errorf("坏 id：%v", err)
		}
		if _, err := f.run(f.admin, "settings rollback", []string{"424242"}, map[string]any{"resource-version": 4}); v1.AsError(err).Code != v1.CodeNotFound {
			t.Errorf("不存在：%v", err)
		}
		st := store.New(bdb, schema.Default())
		other := &model.ConfigSnapshot{ObjectKind: "Inbound", ObjectID: 3, ObjectVersion: 1, Content: json.RawMessage(`{}`), ContentHash: "x", Source: "apply", Status: core.StatusSaved}
		if err := st.Insert(context.Background(), other); err != nil {
			t.Fatal(err)
		}
		if _, err := f.run(f.admin, "settings rollback", []string{jsonID(other.ID)}, map[string]any{"resource-version": 4}); v1.AsError(err).Code != v1.CodeNotFound {
			t.Errorf("别的 kind：%v", err)
		}
		// 快照里混进非 spec 字段：写回时重过分档。
		tainted := &model.ConfigSnapshot{ObjectKind: core.KindName, ObjectID: 1, ObjectVersion: 4, Content: json.RawMessage(`{"heartbeat_interval":45,"master_url":"https://evil.example"}`), ContentHash: "x", Source: core.SourceSettings, Status: core.StatusSaved}
		if err := st.Insert(context.Background(), tainted); err != nil {
			t.Fatal(err)
		}
		_, err = f.run(f.admin, "settings rollback", []string{jsonID(tainted.ID)}, map[string]any{"resource-version": 4})
		e := wantCode(t, err, v1.CodeFieldNotApplyable)
		if !strings.Contains(e.Reason, "master_url") {
			t.Errorf("应当点名 master_url：%s", e.Reason)
		}
		if got := f.show(); got.Metadata.ResourceVersion != 4 || got.Spec.HeartbeatInterval != 30 {
			t.Fatalf("被拒的回滚不该改任何东西：%+v", got.Metadata)
		}
		// 过期版本。
		if _, err := f.run(f.admin, "settings rollback", []string{jsonID(second.ID)}, map[string]any{"resource-version": 1}); v1.AsError(err).Code != v1.CodeVersionConflict {
			t.Errorf("过期版本：%v", err)
		}
		// json 字段里存的是一个 JSON 字符串、int 字段是超过 2^53 的整数：快照与回滚都原样保留。
		f.mustSet(map[string]any{"reality_domains": `"abc"`, "user_quota_override": "9007199254740993"}, 4)
		f.mustSet(map[string]any{"heartbeat_interval": "45"}, 5) // 这份写前快照里含上面两个值
		res, err = f.run(f.admin, "settings snapshots list", nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		latest := res.(*command.PageResult).Items[0].(core.SnapshotMeta)
		res, err = f.run(f.admin, "settings rollback", []string{jsonID(latest.ID)}, map[string]any{"resource-version": 6})
		if err != nil {
			t.Fatalf("回滚含 JSON 字符串与大整数的快照：%v", err)
		}
		obj = res.(*Object)
		if string(obj.Spec.RealityDomains) != `"abc"` || obj.Spec.UserQuotaOverride != 9007199254740993 || obj.Spec.HeartbeatInterval != 30 {
			t.Fatalf("回滚应当原样恢复：%s %d %d", obj.Spec.RealityDomains, obj.Spec.UserQuotaOverride, obj.Spec.HeartbeatInterval)
		}
	})
}

// rules 表里的每个字段都在目录里，校验函数的类型断言与目录类型一致（不一致会在处理请求时 panic）。
func TestRulesMatchCatalog(t *testing.T) {
	table, _ := schema.Default().Table("system_config")
	types := map[string]schema.Type{}
	for _, c := range table.KindColumns() {
		types[c.Name] = c.Type
	}
	for name, rule := range rules {
		typ, ok := types[name]
		if !ok {
			t.Errorf("规则表里的 %s 不在目录里", name)
			continue
		}
		var sample any
		switch typ {
		case schema.TypeBool:
			sample = false
		case schema.TypeInt:
			sample = int64(0)
		case schema.TypeText:
			sample = ""
		case schema.TypeJSON:
			sample = json.RawMessage(`[]`)
		}
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("%s 的规则对 %T 类型的值 panic：%v", name, sample, r)
				}
			}()
			_ = rule(sample)
		}()
	}
}

func jsonID(id int64) string {
	raw, _ := json.Marshal(id)
	return string(raw)
}

// master-settings「主控地址是人类专属的设置写」（当场验证在 authz，这里只测处理函数）。
func TestMasterURL(t *testing.T) {
	dbtest.ForEach(t, func(t *testing.T, bdb *bun.DB) {
		f := setup(t, bdb)
		f.mustSet(map[string]any{"heartbeat_interval": "45"}, 1) // 留一条快照，之后数它不变
		res, err := f.run(f.admin, "settings master-url set", nil, map[string]any{"url": "https://panel.example.com/", "resource-version": 2})
		if err != nil {
			t.Fatal(err)
		}
		obj := res.(*Object)
		if obj.Metadata.ResourceVersion != 3 || obj.Status.MasterURL != "https://panel.example.com" || obj.Status.SubscriptionURL != "" || f.snapshots() != 1 {
			t.Fatalf("改主控地址：%+v %+v %d", obj.Metadata, obj.Status, f.snapshots())
		}
		res, err = f.run(f.admin, "settings master-url set", nil, map[string]any{"subscription-url": "http://Sub.example.com:8080", "resource-version": 3})
		if err != nil || res.(*Object).Status.SubscriptionURL != "http://Sub.example.com:8080" || res.(*Object).Metadata.ResourceVersion != 4 {
			t.Fatalf("订阅域名带端口：%v %+v", err, res)
		}
		for _, bad := range []string{"https://panel.example.com/admin", "panel.example.com", "https://user@panel.example.com", "https://panel.example.com/?x=1", "https://panel.example.com/#a", "ftp://panel.example.com", "https://", "https://panel.example.com:"} {
			_, err := f.run(f.admin, "settings master-url set", nil, map[string]any{"url": bad, "resource-version": 4})
			e := wantCode(t, err, v1.CodeBadRequest)
			if !strings.Contains(e.Reason, "url") {
				t.Errorf("%s：reason 应当含 url：%s", bad, e.Reason)
			}
		}
		if got := f.show(); got.Status.MasterURL != "https://panel.example.com" || got.Metadata.ResourceVersion != 4 {
			t.Fatalf("形状不对的没有写进去：%+v", got.Status)
		}
		if _, err := f.run(f.admin, "settings master-url set", nil, map[string]any{"resource-version": 4}); v1.AsError(err).Code != v1.CodeBadRequest {
			t.Errorf("两个都不给：%v", err)
		}
		if _, err := f.run(f.admin, "settings master-url set", nil, map[string]any{"url": "https://a.example"}); v1.AsError(err).Code != v1.CodeBadRequest {
			t.Errorf("缺版本：%v", err)
		}
		if _, err := f.run(f.admin, "settings master-url set", nil, map[string]any{"url": "https://a.example", "resource-version": 3}); v1.AsError(err).Code != v1.CodeVersionConflict {
			t.Errorf("过期版本：%v", err)
		}
		// 空串清掉。
		res, err = f.run(f.admin, "settings master-url set", nil, map[string]any{"url": "", "resource-version": 4})
		if err != nil || res.(*Object).Status.MasterURL != "" || res.(*Object).Metadata.ResourceVersion != 5 || f.snapshots() != 1 {
			t.Fatalf("清掉：%v %+v %d", err, res, f.snapshots())
		}
		if _, err := f.run(f.user, "settings master-url set", nil, map[string]any{"url": "https://a.example", "resource-version": 5}); v1.AsError(err).Code != v1.CodeForbidden {
			t.Errorf("普通用户：%v", err)
		}
	})
}
