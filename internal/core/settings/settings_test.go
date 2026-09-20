package settings

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sync"
	"testing"

	"github.com/uptrace/bun"

	"github.com/satchel/satchel/internal/base/db/dbtest"
	"github.com/satchel/satchel/internal/base/model"
	"github.com/satchel/satchel/internal/base/schema"
	"github.com/satchel/satchel/internal/base/store"
	v1 "github.com/satchel/satchel/pkg/api/v1"
)

func repo(t *testing.T, bdb *bun.DB) *Repo {
	t.Helper()
	return New(bdb, store.New(bdb, schema.Default()), schema.Default())
}

func wantCode(t *testing.T, err error, code v1.Code) *v1.Error {
	t.Helper()
	var e *v1.Error
	if !errors.As(err, &e) || e.Code != code {
		t.Fatalf("想要错误码 %s，得到 %v", code, err)
	}
	return e
}

// master-settings「settings show」：每个 key 都有默认值，类型与目录一致，没有目录外的项。
func TestDefaultsCoverCatalog(t *testing.T) {
	table, _ := schema.Default().Table("system_config")
	if len(table.Settings) != 92 {
		t.Fatalf("目录应当 92 个 key，得到 %d", len(table.Settings))
	}
	seen := map[string]bool{}
	for _, k := range table.Settings {
		seen[k.Name] = true
		v, ok := defaults[k.Name]
		if !ok {
			t.Errorf("key %s 没有默认值", k.Name)
			continue
		}
		switch k.Type {
		case schema.TypeBool:
			if _, ok := v.(bool); !ok {
				t.Errorf("%s 的默认值应当是 bool，得到 %T", k.Name, v)
			}
		case schema.TypeInt:
			if _, ok := v.(int64); !ok {
				t.Errorf("%s 的默认值应当是 int64，得到 %T", k.Name, v)
			}
		case schema.TypeText:
			if _, ok := v.(string); !ok {
				t.Errorf("%s 的默认值应当是 string，得到 %T", k.Name, v)
			}
		case schema.TypeJSON:
			raw, ok := v.(json.RawMessage)
			if !ok || !json.Valid(raw) {
				t.Errorf("%s 的默认值应当是合法的 json.RawMessage，得到 %T %s", k.Name, v, v)
			}
		}
	}
	for name := range defaults {
		if !seen[name] {
			t.Errorf("默认值表里的 %s 不在目录里", name)
		}
	}
	// spec 里钉住的几个值。
	for name, want := range map[string]any{
		"dashboard_refresh_interval_ms": int64(5000), "probe_disguise_ping_interval_ms": int64(60000), "default_theme": "flat",
		"probe_disguise_metric_traffic": true, "probe_disguise_show_name": false, "update_cdn_enabled": true, "probe_disguise_theme": "follow",
		"master_recovery_url": "", "primary_admin_username": "", "user_quota_routed_outbound": int64(2),
	} {
		if defaults[name] != want {
			t.Errorf("%s 的默认值应当是 %v，得到 %v", name, want, defaults[name])
		}
	}
}

// storage-schema「Settings key catalog」的编码约定：四种类型往返，mmwx 三种布尔写法都能读，不规范的整数拒绝。
func TestEncoding(t *testing.T) {
	for raw, want := range map[string]bool{"": false, "0": false, "false": false, "1": true, "true": true} {
		if got, err := Decode(schema.TypeBool, raw); err != nil || got != want {
			t.Errorf("bool %q 应当读成 %v：%v %v", raw, want, got, err)
		}
	}
	for _, bad := range []string{"yes", "TRUE", " 1", "2"} {
		if _, err := Decode(schema.TypeBool, bad); err == nil || v1.AsError(err).Code != v1.CodeBadRequest {
			t.Errorf("bool %q 应当 bad_request：%v", bad, err)
		}
	}
	for raw, want := range map[string]int64{"30": 30, "0": 0, "-1": -1, "60000": 60000} {
		if got, err := Decode(schema.TypeInt, raw); err != nil || got != want {
			t.Errorf("int %q 应当读成 %d：%v %v", raw, want, got, err)
		}
	}
	for _, bad := range []string{"01", "30 ", "+1", "1.5", "abc", "", "99999999999999999999"} {
		if _, err := Decode(schema.TypeInt, bad); err == nil || v1.AsError(err).Code != v1.CodeBadRequest {
			t.Errorf("int %q 应当 bad_request：%v", bad, err)
		}
	}
	if got, err := Decode(schema.TypeJSON, `[1, 2]`); err != nil || string(got.(json.RawMessage)) != `[1, 2]` {
		t.Errorf("json 应当原文保留：%v %v", got, err)
	}
	if _, err := Decode(schema.TypeJSON, "not json"); err == nil {
		t.Error("坏 JSON 应当拒绝")
	}
	if got, err := Decode(schema.TypeText, " x=y "); err != nil || got != " x=y " {
		t.Errorf("文本原样：%v %v", got, err)
	}
	for _, tc := range []struct {
		t    schema.Type
		v    any
		want string
	}{{schema.TypeBool, true, "true"}, {schema.TypeBool, false, "false"}, {schema.TypeInt, int64(45), "45"}, {schema.TypeText, "a", "a"}, {schema.TypeJSON, json.RawMessage(`{"a":1}`), `{"a":1}`}} {
		if got, err := Encode(tc.t, tc.v); err != nil || got != tc.want {
			t.Errorf("Encode(%v)=%q %v，想要 %q", tc.v, got, err, tc.want)
		}
	}
	if _, err := Encode(schema.TypeInt, 45); err == nil {
		t.Error("Encode 只收固定的 Go 类型（int64），int 应当报内部错误")
	}
}

// master-settings「settings set」：字符串与原生类型都收，对不上点名。
func TestNormalize(t *testing.T) {
	cases := []struct {
		t    schema.Type
		v    any
		want any
		bad  bool
	}{
		{schema.TypeInt, "45", int64(45), false},
		{schema.TypeInt, float64(45), int64(45), false},
		{schema.TypeInt, 45, int64(45), false},
		{schema.TypeInt, 45.5, nil, true},
		{schema.TypeInt, true, nil, true},
		{schema.TypeInt, "30 ", nil, true},
		{schema.TypeBool, "1", true, false},
		{schema.TypeBool, false, false, false},
		{schema.TypeBool, float64(1), nil, true},
		{schema.TypeText, "x", "x", false},
		{schema.TypeText, float64(5), nil, true},
		{schema.TypeJSON, "[1,2]", json.RawMessage("[1,2]"), false},
		{schema.TypeJSON, "not json", nil, true},
	}
	for _, tc := range cases {
		got, err := Normalize(tc.t, tc.v)
		if tc.bad {
			if err == nil || v1.AsError(err).Code != v1.CodeBadRequest {
				t.Errorf("Normalize(%s, %v) 应当 bad_request：%v", tc.t, tc.v, err)
			}
			continue
		}
		if err != nil {
			t.Errorf("Normalize(%s, %v)：%v", tc.t, tc.v, err)
			continue
		}
		if raw, ok := tc.want.(json.RawMessage); ok {
			if string(got.(json.RawMessage)) != string(raw) {
				t.Errorf("Normalize(%s, %v)=%s，想要 %s", tc.t, tc.v, got, raw)
			}
			continue
		}
		if got != tc.want {
			t.Errorf("Normalize(%s, %v)=%v (%T)，想要 %v (%T)", tc.t, tc.v, got, got, tc.want, tc.want)
		}
	}
	got, err := Normalize(schema.TypeJSON, []any{float64(1), "a"})
	if err != nil || string(got.(json.RawMessage)) != `[1,"a"]` {
		t.Fatalf("原生 JSON 值应当编成 RawMessage：%s %v", got, err)
	}
}

// master-settings「单例行的建立」与「settings show」：空库建行、幂等、默认值与分档、只读项恒真。
func TestEnsureSingletonAndLoad(t *testing.T) {
	dbtest.ForEach(t, func(t *testing.T, bdb *bun.DB) {
		ctx := context.Background()
		r := repo(t, bdb)
		wantCode(t, func() error { _, err := r.Load(ctx); return err }(), v1.CodeNotFound)
		for i := 0; i < 2; i++ {
			if err := r.EnsureSingleton(ctx); err != nil {
				t.Fatal(err)
			}
		}
		n, _ := bdb.NewSelect().Model((*model.SystemSettings)(nil)).Count(ctx)
		if n != 1 {
			t.Fatalf("应当恰好一行，得到 %d", n)
		}
		st, err := r.Load(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if st.Version != 1 || st.CreatedAt.IsZero() || st.UpdatedAt.IsZero() {
			t.Fatalf("单例行应当版本 1 带时间戳：%+v", st)
		}
		if len(st.Values) != 46+92 {
			t.Fatalf("字段值表应当 138 项，得到 %d", len(st.Values))
		}
		for name, want := range map[string]any{
			"heartbeat_interval": int64(30), "enable_short_link": true, "enable_miaomiaowu_features": true, "sub_info_expire_prefix": "📅过期时间",
			"notify_daily_traffic_time": "08:00", "subscription_output_format": "yaml", "silent_mode_timeout": int64(15), "telegram_bot_token": "",
			"dashboard_refresh_interval_ms": int64(5000), "probe_disguise_ping_interval_ms": int64(60000), "default_theme": "flat",
			"probe_disguise_metric_traffic": true, "probe_disguise_show_name": false, "branding_site_title": "",
			"master_url": "", "update_cdn_enabled": true, "require_encryption": true, "master_https_recovery_pending": false,
		} {
			if st.Values[name] != want {
				t.Errorf("%s 应当是 %v (%T)，得到 %v (%T)", name, want, want, st.Values[name], st.Values[name])
			}
		}
		if string(st.Values["probe_disguise_server_ids"].(json.RawMessage)) != "[]" {
			t.Errorf("json 类型的默认值：%s", st.Values["probe_disguise_server_ids"])
		}
		// 键值表里 mmwx 的三种布尔写法与坏值。
		for k, v := range map[string]string{"probe_disguise_enabled": "1", "sub_rate_enabled": "true", "block_unknown_subscription_ua": "", "require_encryption": "false", "dashboard_refresh_interval_ms": "abc"} {
			if _, err := bdb.NewInsert().Model(&model.SystemSettingEntry{Key: k, Value: v}).Exec(ctx); err != nil {
				t.Fatal(err)
			}
		}
		st, err = r.Load(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if st.Values["probe_disguise_enabled"] != true || st.Values["sub_rate_enabled"] != true || st.Values["block_unknown_subscription_ua"] != false {
			t.Errorf("三种布尔写法：%v %v %v", st.Values["probe_disguise_enabled"], st.Values["sub_rate_enabled"], st.Values["block_unknown_subscription_ua"])
		}
		if st.Values["require_encryption"] != true {
			t.Error("require_encryption 恒为 true")
		}
		if st.Values["dashboard_refresh_interval_ms"] != int64(5000) {
			t.Errorf("坏值按默认读：%v", st.Values["dashboard_refresh_interval_ms"])
		}
		f, ok := r.Field("heartbeat_interval")
		if !ok || !f.Column || f.Class != schema.ClassSpec || f.Type != schema.TypeInt {
			t.Errorf("heartbeat_interval 的字段描述不对：%+v", f)
		}
		if f, _ := r.Field("master_url"); f.Column || f.Class != schema.ClassHuman {
			t.Errorf("master_url 的字段描述不对：%+v", f)
		}
		if f, _ := r.Field("probe_external_token_sha256"); !f.Masked || f.Class != schema.ClassSpec {
			t.Errorf("probe_external_token_sha256 应当是打码的 spec key：%+v", f)
		}
		if _, ok := r.Field("id"); ok {
			t.Error("元数据列不是字段")
		}
		if len(r.Fields()) != 138 {
			t.Errorf("Fields 应当 138 项，得到 %d", len(r.Fields()))
		}
	})
}

func TestEnsureSingletonConcurrent(t *testing.T) {
	dbtest.ForEach(t, func(t *testing.T, bdb *bun.DB) {
		r := repo(t, bdb)
		var wg sync.WaitGroup
		errs := make(chan error, 10)
		for i := 0; i < 10; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				errs <- r.EnsureSingleton(context.Background())
			}()
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			if err != nil {
				t.Errorf("并发建行不该报错：%v", err)
			}
		}
		n, _ := bdb.NewSelect().Model((*model.SystemSettings)(nil)).Count(context.Background())
		if n != 1 {
			t.Fatalf("应当恰好一行，得到 %d", n)
		}
	})
}

func snapshotCount(t *testing.T, bdb *bun.DB) int {
	t.Helper()
	n, err := bdb.NewSelect().Model((*model.ConfigSnapshot)(nil)).Count(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// master-settings「settings set 是日常运维档的事务写」与「每次写都存一份写前快照」的仓储部分。
func TestWrite(t *testing.T) {
	dbtest.ForEach(t, func(t *testing.T, bdb *bun.DB) {
		ctx := context.Background()
		r := repo(t, bdb)
		if err := r.EnsureSingleton(ctx); err != nil {
			t.Fatal(err)
		}
		wantCode(t, func() error { _, err := r.Write(ctx, WriteRequest{ExpectedVersion: 1}); return err }(), v1.CodeBadRequest)
		wantCode(t, func() error {
			_, err := r.Write(ctx, WriteRequest{Values: map[string]any{"nosuch": "x"}, ExpectedVersion: 1})
			return err
		}(), v1.CodeUnknownField)
		// 只读档的 key 在仓储层就没有写路径（与只读列同一条口径）；运行态的 key 是系统写，仓储层放行。
		wantCode(t, func() error {
			_, err := r.Write(ctx, WriteRequest{Values: map[string]any{"require_encryption": false}, ExpectedVersion: 1})
			return err
		}(), v1.CodeFieldNotApplyable)
		if st, _ := r.Load(ctx); st.Version != 1 {
			t.Fatalf("被拒的写不该抬版本：%d", st.Version)
		}

		// 列与 key 一起改。
		st, err := r.Write(ctx, WriteRequest{Values: map[string]any{"heartbeat_interval": int64(45), "branding_site_title": "Satchel"}, ExpectedVersion: 1, Snapshot: true, Source: SourceSettings})
		if err != nil {
			t.Fatal(err)
		}
		if st.Version != 2 || st.Values["heartbeat_interval"] != int64(45) || st.Values["branding_site_title"] != "Satchel" {
			t.Fatalf("写后：%d %v %v", st.Version, st.Values["heartbeat_interval"], st.Values["branding_site_title"])
		}
		var row model.SystemSettings
		if err := bdb.NewSelect().Model(&row).Where("id = 1").Scan(ctx); err != nil || row.HeartbeatInterval != 45 || row.ResourceVersion != 2 {
			t.Fatalf("行应当落地：%v %+v", err, row)
		}
		var entry model.SystemSettingEntry
		if err := bdb.NewSelect().Model(&entry).Where("key = ?", "branding_site_title").Scan(ctx); err != nil || entry.Value != "Satchel" || entry.UpdatedAt.IsZero() {
			t.Fatalf("key 应当落地：%v %+v", err, entry)
		}
		// 快照：写前内容、哈希、来源、状态。
		var snaps []model.ConfigSnapshot
		if err := bdb.NewSelect().Model(&snaps).Scan(ctx); err != nil || len(snaps) != 1 {
			t.Fatalf("应当恰好一条快照：%v %d", err, len(snaps))
		}
		s := snaps[0]
		if s.ObjectKind != KindName || s.ObjectID != 1 || s.ObjectVersion != 1 || s.Source != SourceSettings || s.Status != StatusSaved || s.ApplyID != nil {
			t.Fatalf("快照元数据不对：%+v", s)
		}
		var content map[string]any
		if err := json.Unmarshal(s.Content, &content); err != nil {
			t.Fatal(err)
		}
		if content["heartbeat_interval"] != float64(30) || content["branding_site_title"] != "" || content["probe_disguise_show_name"] != false {
			t.Errorf("快照内容应当是写前的日常运维档：%v", content)
		}
		for _, absent := range []string{"master_url", "require_encryption", "update_cdn_enabled", "telegram_chat_id", "silent_mode", "master_https_recovery_pending", "id", "resource_version"} {
			if _, ok := content[absent]; ok {
				t.Errorf("快照内容里不该有 %s", absent)
			}
		}
		if len(content) != 100 {
			t.Errorf("快照应当含 100 个 spec 字段，得到 %d", len(content))
		}
		// content_hash 是 canonical 字节的 SHA-256：PostgreSQL 的 jsonb 读回来的字节可能被重排，规范化后一定对得上。
		canonical, err := Canonical(s.Content)
		if err != nil {
			t.Fatal(err)
		}
		if s.ContentHash != ContentHash(canonical) {
			t.Error("content_hash 应当是 canonical 内容的 SHA-256")
		}
		if again, _ := Canonical(canonical); string(again) != string(canonical) {
			t.Error("规范化应当是幂等的")
		}
		sum := sha256.Sum256(canonical)
		if s.ContentHash != hex.EncodeToString(sum[:]) {
			t.Error("ContentHash 就是 SHA-256 十六进制")
		}

		// 过期版本：整个事务回滚，快照不多。
		e := wantCode(t, func() error {
			_, err := r.Write(ctx, WriteRequest{Values: map[string]any{"heartbeat_interval": int64(50)}, ExpectedVersion: 1, Snapshot: true, Source: SourceSettings})
			return err
		}(), v1.CodeVersionConflict)
		if e.State["resourceVersion"] != int64(2) {
			t.Errorf("state 应当带当前版本 2：%v", e.State)
		}
		if snapshotCount(t, bdb) != 1 {
			t.Error("失败的写不该留下快照")
		}
		if st, _ := r.Load(ctx); st.Values["heartbeat_interval"] != int64(45) {
			t.Error("失败的写不该改值")
		}
		// force 只跳过比对。
		st, err = r.Write(ctx, WriteRequest{Values: map[string]any{"heartbeat_interval": int64(50)}, ExpectedVersion: 1, Force: true, Snapshot: true, Source: SourceSettings})
		if err != nil || st.Version != 3 || st.Values["heartbeat_interval"] != int64(50) || snapshotCount(t, bdb) != 2 {
			t.Fatalf("force：%v %+v %d", err, st, snapshotCount(t, bdb))
		}
		// 只改 key 也抬版本（Bump 路径）。
		st, err = r.Write(ctx, WriteRequest{Values: map[string]any{"branding_brand_title": "B"}, ExpectedVersion: 3, Snapshot: true, Source: SourceRollback})
		if err != nil || st.Version != 4 || st.Values["branding_brand_title"] != "B" {
			t.Fatalf("只改 key：%v %+v", err, st)
		}
		// 人类专属 key：抬版本、不存快照。
		st, err = r.Write(ctx, WriteRequest{Values: map[string]any{"master_url": "https://panel.example.com"}, ExpectedVersion: 4})
		if err != nil || st.Version != 5 || st.Values["master_url"] != "https://panel.example.com" || snapshotCount(t, bdb) != 3 {
			t.Fatalf("人类专属 key：%v %+v %d", err, st, snapshotCount(t, bdb))
		}
		// 人类专属列与主控自身类列走各自的写入原语，同样抬版本。
		st, err = r.Write(ctx, WriteRequest{Values: map[string]any{"silent_mode_timeout": int64(20)}, ExpectedVersion: 5})
		if err != nil || st.Version != 6 || st.Values["silent_mode_timeout"] != int64(20) {
			t.Fatalf("人类专属列：%v %+v", err, st)
		}
		st, err = r.Write(ctx, WriteRequest{Values: map[string]any{"telegram_chat_id": "123"}, ExpectedVersion: 6})
		if err != nil || st.Version != 7 || st.Values["telegram_chat_id"] != "123" {
			t.Fatalf("主控自身类列：%v %+v", err, st)
		}
		// 过期版本对只改 key 的写同样拒绝。
		wantCode(t, func() error {
			_, err := r.Write(ctx, WriteRequest{Values: map[string]any{"master_url": ""}, ExpectedVersion: 5})
			return err
		}(), v1.CodeVersionConflict)
		// json 类型的 key 往返。
		st, err = r.Write(ctx, WriteRequest{Values: map[string]any{"probe_disguise_server_ids": json.RawMessage(`[1, 2]`)}, ExpectedVersion: 7})
		if err != nil || string(st.Values["probe_disguise_server_ids"].(json.RawMessage)) != `[1, 2]` {
			t.Fatalf("json key：%v %s", err, st.Values["probe_disguise_server_ids"])
		}
		// 再写一次同一个 key 是更新不是插入。
		if _, err := r.Write(ctx, WriteRequest{Values: map[string]any{"branding_brand_title": "C"}, ExpectedVersion: 8}); err != nil {
			t.Fatal(err)
		}
		if n, _ := bdb.NewSelect().Model((*model.SystemSettingEntry)(nil)).Where("key = ?", "branding_brand_title").Count(ctx); n != 1 {
			t.Errorf("同一个 key 应当只有一行，得到 %d", n)
		}
		// 运行态的 key 经仓储层可写（系统路径），同样抬版本。
		if st, err := r.Write(ctx, WriteRequest{Values: map[string]any{"master_https_recovery_pending": true}, ExpectedVersion: 9}); err != nil || st.Values["master_https_recovery_pending"] != true || st.Version != 10 {
			t.Fatalf("运行态 key：%v %+v", err, st)
		}
	})
}

// 快照的 canonical 化不改写大整数，也不把 JSON 字段里的 JSON 字符串当别的东西。
func TestCanonicalKeepsNumbersAndStrings(t *testing.T) {
	in := []byte(`{"b": {"z": 1, "a": 9007199254740993}, "a": "\"abc\"", "n": 1.50}`)
	got, err := Canonical(in)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != `{"a":"\"abc\"","b":{"a":9007199254740993,"z":1},"n":1.50}` {
		t.Fatalf("canonical：%s", got)
	}
	if _, err := Canonical([]byte(`not json`)); err == nil {
		t.Fatal("坏 JSON 应当报错")
	}
}

func TestWriteConcurrentSameVersion(t *testing.T) {
	dbtest.ForEach(t, func(t *testing.T, bdb *bun.DB) {
		ctx := context.Background()
		r := repo(t, bdb)
		if err := r.EnsureSingleton(ctx); err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		results := make(chan error, 10)
		for i := 0; i < 10; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				_, err := r.Write(ctx, WriteRequest{Values: map[string]any{"heartbeat_interval": int64(40 + i)}, ExpectedVersion: 1, Snapshot: true, Source: SourceSettings})
				results <- err
			}(i)
		}
		wg.Wait()
		close(results)
		okCount, conflicts := 0, 0
		for err := range results {
			switch {
			case err == nil:
				okCount++
			case v1.AsError(err).Code == v1.CodeVersionConflict:
				conflicts++
			default:
				t.Errorf("意外的错误：%v", err)
			}
		}
		if okCount != 1 || conflicts != 9 {
			t.Fatalf("应当恰好一个成功、九个冲突，得到 %d / %d", okCount, conflicts)
		}
		st, _ := r.Load(ctx)
		if st.Version != 2 || snapshotCount(t, bdb) != 1 {
			t.Fatalf("版本应当是 2、快照恰好一条：%d %d", st.Version, snapshotCount(t, bdb))
		}
	})
}

// master-settings「每次写都存一份写前快照」的列表与读取。
func TestSnapshots(t *testing.T) {
	dbtest.ForEach(t, func(t *testing.T, bdb *bun.DB) {
		ctx := context.Background()
		r := repo(t, bdb)
		if err := r.EnsureSingleton(ctx); err != nil {
			t.Fatal(err)
		}
		if _, err := r.Write(ctx, WriteRequest{Values: map[string]any{"heartbeat_interval": int64(45)}, ExpectedVersion: 1, Snapshot: true, Source: SourceSettings}); err != nil {
			t.Fatal(err)
		}
		if _, err := r.Write(ctx, WriteRequest{Values: map[string]any{"heartbeat_interval": int64(50)}, ExpectedVersion: 2, Snapshot: true, Source: SourceRollback}); err != nil {
			t.Fatal(err)
		}
		other := &model.ConfigSnapshot{ObjectKind: "Inbound", ObjectID: 3, ObjectVersion: 1, Content: json.RawMessage(`{}`), ContentHash: "x", Source: "apply", Status: StatusSaved}
		if err := store.New(bdb, schema.Default()).Insert(ctx, other); err != nil {
			t.Fatal(err)
		}
		list, err := r.ListSnapshots(ctx, 10, 0)
		if err != nil || len(list) != 2 || list[0].ObjectVersion != 2 || list[1].ObjectVersion != 1 || list[0].Source != SourceRollback || list[0].ContentHash == "" {
			t.Fatalf("列表应当两条、倒序、不含别的 kind：%v %+v", err, list)
		}
		if n, _ := r.CountSnapshots(ctx); n != 2 {
			t.Errorf("总数应当 2，得到 %d", n)
		}
		page, _ := r.ListSnapshots(ctx, 1, list[0].ID)
		if len(page) != 1 || page[0].ID != list[1].ID {
			t.Errorf("keyset 翻页：%+v", page)
		}
		snap, err := r.GetSnapshot(ctx, list[1].ID)
		if err != nil {
			t.Fatal(err)
		}
		var content map[string]any
		if err := json.Unmarshal(snap.Content, &content); err != nil || content["heartbeat_interval"] != float64(30) {
			t.Fatalf("第一份快照的内容应当是版本 1 的值：%v %v", err, content)
		}
		wantCode(t, func() error { _, err := r.GetSnapshot(ctx, 424242); return err }(), v1.CodeNotFound)
		wantCode(t, func() error { _, err := r.GetSnapshot(ctx, other.ID); return err }(), v1.CodeNotFound)
	})
}
