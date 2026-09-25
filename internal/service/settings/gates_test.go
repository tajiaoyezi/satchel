package settings

import (
	"context"
	"encoding/json"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/uptrace/bun"

	"github.com/satchel/satchel/internal/base/db/dbtest"
	"github.com/satchel/satchel/internal/base/schema"
	"github.com/satchel/satchel/internal/command"
	core "github.com/satchel/satchel/internal/core/settings"
	v1 "github.com/satchel/satchel/pkg/api/v1"
)

// master-settings「门的设置是人类专属的设置写」：门这一组恰好是 spec 列出的 15 个字段，都是人类专属档；
// 命令表里 --set 的说明列出了同样的 15 个（explain 看得到），两处由这个测试钉在一起。
func TestGateFieldsCatalog(t *testing.T) {
	want := strings.Fields("master_local_only silent_mode silent_mode_timeout probe_disguise_block_login brute_force_enabled " +
		"brute_force_max_failures brute_force_window_minutes brute_force_block_minutes login_rate_max_attempts " +
		"login_rate_window_minutes login_rate_lock_minutes skip_local_ip turnstile_site_key turnstile_secret_key trusted_proxies")
	var got []string
	for name := range gateFields {
		got = append(got, name)
	}
	sort.Strings(got)
	sort.Strings(want)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("门这一组的字段不对：%v", got)
	}
	dbtest.ForEach(t, func(t *testing.T, bdb *bun.DB) {
		f := setup(t, bdb)
		for _, name := range want {
			field, ok := f.svc.repo.Field(name)
			if !ok || field.Class != schema.ClassHuman {
				t.Errorf("%s 应当是人类专属档：%+v", name, field)
			}
		}
	})
	c, _ := command.Catalog().Lookup("settings gates set")
	flag, _ := c.FlagByName("set")
	for _, name := range want {
		if !strings.Contains(flag.Description, name) {
			t.Errorf("settings gates set 的 --set 说明里缺 %s", name)
		}
	}
}

func (f *fixture) gates(ctx context.Context, values map[string]any, version int) (*Object, error) {
	f.t.Helper()
	res, err := f.run(ctx, "settings gates set", nil, map[string]any{"set": values, "resource-version": version})
	if err != nil {
		return nil, err
	}
	return typed(f.t, res), nil
}

func TestGatesSet(t *testing.T) {
	dbtest.ForEach(t, func(t *testing.T, bdb *bun.DB) {
		f := setup(t, bdb)
		var seen []int64
		f.svc.OnWrite(func(st *core.State) { seen = append(seen, st.Version) })

		obj, err := f.gates(f.admin, map[string]any{"silent_mode": true, "silent_mode_timeout": "30"}, 1)
		if err != nil {
			t.Fatal(err)
		}
		if !obj.Status.SilentMode || obj.Status.SilentModeTimeout != 30 || obj.Metadata.ResourceVersion != 2 {
			t.Fatalf("写后的对象不对：%+v %+v", obj.Status.SilentMode, obj.Metadata)
		}
		if n, _ := f.svc.repo.CountSnapshots(context.Background()); n != 0 {
			t.Fatalf("门的字段不进快照，得到 %d 条", n)
		}
		if len(seen) != 1 || seen[0] != 2 {
			t.Fatalf("写成功后应当回调一次、交出版本 2：%v", seen)
		}

		// 不属于门的字段整单拒绝：点名字段，别的人类专属字段指向它自己的命令。
		_, err = f.gates(f.admin, map[string]any{"silent_mode": false, "master_url": "https://a.example"}, 2)
		e := v1.AsError(err)
		if e.Code != v1.CodeFieldNotApplyable || !strings.Contains(e.Reason, "master_url") || !strings.Contains(e.Reason, "不属于门这一组") || !strings.Contains(e.Next, "settings master-url set") {
			t.Fatalf("master_url 应当 field_not_applyable 并指路：%v", err)
		}
		_, err = f.gates(f.admin, map[string]any{"heartbeat_interval": 45}, 2)
		if e := v1.AsError(err); e.Code != v1.CodeFieldNotApplyable || !strings.Contains(e.Reason, "heartbeat_interval") {
			t.Fatalf("日常运维字段应当 field_not_applyable：%v", err)
		}
		if _, err := f.gates(f.admin, map[string]any{"nosuch": 1}, 2); v1.AsError(err).Code != v1.CodeUnknownField {
			t.Fatalf("不存在的字段应当 unknown_field：%v", err)
		}
		if _, err := f.gates(f.admin, map[string]any{}, 2); v1.AsError(err).Code != v1.CodeBadRequest {
			t.Fatalf("空的 --set 应当 bad_request：%v", err)
		}
		// 字段规则照旧。
		if _, err := f.gates(f.admin, map[string]any{"login_rate_max_attempts": 0}, 2); v1.AsError(err).Code != v1.CodeBadRequest {
			t.Fatalf("次数至少为 1：%v", err)
		}
		if _, err := f.gates(f.admin, map[string]any{"turnstile_site_key": "admin"}, 2); v1.AsError(err).Code != v1.CodeBadRequest {
			t.Fatalf("site key 非空时至少 20 个字符：%v", err)
		}
		// 版本冲突、普通用户。
		if _, err := f.gates(f.admin, map[string]any{"silent_mode": false}, 1); v1.AsError(err).Code != v1.CodeVersionConflict {
			t.Fatalf("旧版本应当 version_conflict：%v", err)
		}
		if _, err := f.gates(f.user, map[string]any{"silent_mode": false}, 2); v1.AsError(err).Code != v1.CodeForbidden {
			t.Fatalf("普通用户应当 forbidden：%v", err)
		}
		if len(seen) != 1 {
			t.Fatalf("失败的写不回调：%v", seen)
		}
		st, _ := f.svc.repo.Load(context.Background())
		if st.Version != 2 || st.Values["silent_mode"] != true {
			t.Fatalf("被拒的写不该改任何东西：%d %v", st.Version, st.Values["silent_mode"])
		}

		// 打码字段：*** 表示不变，空串清掉。
		if _, err := f.gates(f.admin, map[string]any{"turnstile_site_key": "0x4AAAAAAAsitekeyforsatchel", "turnstile_secret_key": "0x4AAAAAAAsecret"}, 2); err != nil {
			t.Fatal(err)
		}
		obj, err = f.gates(f.admin, map[string]any{"turnstile_secret_key": v1.Redacted, "skip_local_ip": false}, 3)
		if err != nil || string(obj.Status.TurnstileSecretKey) != "0x4AAAAAAAsecret" || obj.Status.SkipLocalIP {
			t.Fatalf("*** 应当保持原值：%+v %v", obj, err)
		}
		obj, err = f.gates(f.admin, map[string]any{"turnstile_secret_key": ""}, 4)
		if err != nil || string(obj.Status.TurnstileSecretKey) != "" {
			t.Fatalf("空串应当清掉：%v", err)
		}
		if len(seen) != 4 {
			t.Fatalf("每次写成功都回调：%v", seen)
		}
	})
}

// master-settings「门的设置是人类专属的设置写」：反代登记的形状。
func TestGatesTrustedProxies(t *testing.T) {
	dbtest.ForEach(t, func(t *testing.T, bdb *bun.DB) {
		f := setup(t, bdb)
		for _, bad := range []string{
			`{"cidr":"127.0.0.1"}`,
			`[{"cidr":"not-a-net","header":"X-Real-IP"}]`,
			`[{"cidr":"10.0.0.0/8","header":"X-Client-IP"}]`,
			`[{"cidr":"10.0.0.0/8","header":"X-Real-IP","note":"x"}]`,
		} {
			_, err := f.gates(f.admin, map[string]any{"trusted_proxies": bad}, 1)
			if e := v1.AsError(err); e.Code != v1.CodeBadRequest || !strings.Contains(e.Reason, "trusted_proxies") {
				t.Errorf("%s 应当 bad_request 并点名 trusted_proxies：%v", bad, err)
			}
		}
		good := `[{"cidr":"127.0.0.1","header":"cf-connecting-ip"},{"cidr":"10.0.0.0/8","header":"X-Forwarded-For"}]`
		obj, err := f.gates(f.admin, map[string]any{"trusted_proxies": good}, 1)
		if err != nil {
			t.Fatal(err)
		}
		var list []map[string]string
		if err := json.Unmarshal(obj.Status.TrustedProxies, &list); err != nil || len(list) != 2 || list[0]["cidr"] != "127.0.0.1" || list[1]["header"] != "X-Forwarded-For" {
			t.Fatalf("status.trusted_proxies 应当是这两项：%s %v", obj.Status.TrustedProxies, err)
		}
		// REST 上给原生的 JSON 数组也收。
		if _, err := f.gates(f.admin, map[string]any{"trusted_proxies": []any{map[string]any{"cidr": "192.0.2.0/24", "header": "X-Real-IP"}}}, 2); err != nil {
			t.Fatalf("原生 JSON 数组应当收：%v", err)
		}
		st, _ := f.svc.repo.Load(context.Background())
		if g := st.Gates(); len(g.TrustedProxies) != 1 || g.TrustedProxies[0].Header != core.HeaderXRealIP {
			t.Fatalf("门的视图应当读到新的登记：%+v", g.TrustedProxies)
		}
	})
}

// master-settings：日常运维的写不收门的字段；回滚与主控地址的写同样回调。
func TestOtherWritesNotify(t *testing.T) {
	dbtest.ForEach(t, func(t *testing.T, bdb *bun.DB) {
		f := setup(t, bdb)
		n := 0
		f.svc.OnWrite(func(*core.State) { n++ })
		_, err := f.set(map[string]any{"skip_local_ip": "false"}, 1)
		if e := v1.AsError(err); e.Code != v1.CodeFieldNotApplyable || !strings.Contains(e.Reason, "skip_local_ip") || !strings.Contains(e.Reason, "人类专属") {
			t.Fatalf("settings set 不收门的字段：%v", err)
		}
		if _, err := f.set(map[string]any{"heartbeat_interval": "45"}, 1); err != nil {
			t.Fatal(err)
		}
		snaps, _ := f.svc.repo.ListSnapshots(context.Background(), 10, 0)
		if _, err := f.run(f.admin, "settings rollback", []string{strconv.FormatInt(snaps[0].ID, 10)}, map[string]any{"resource-version": 2}); err != nil {
			t.Fatal(err)
		}
		if _, err := f.run(f.admin, "settings master-url set", nil, map[string]any{"url": "https://panel.example.com", "resource-version": 3}); err != nil {
			t.Fatal(err)
		}
		if n != 3 {
			t.Fatalf("set、rollback、master-url set 各回调一次，得到 %d", n)
		}
	})
}
