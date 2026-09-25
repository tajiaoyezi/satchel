// Package settings 是业务层的系统设置（master-settings）：settings show / set / snapshots list / rollback / master-url set 五条命令，
// 写请求的字段归一、分档检查与字段规则，管理员可见范围。事务与存储在 core/settings。
package settings

import (
	"bytes"
	"context"
	"encoding/json"
	"sort"
	"strconv"

	"github.com/satchel/satchel/internal/base/schema"
	"github.com/satchel/satchel/internal/command"
	core "github.com/satchel/satchel/internal/core/settings"
	v1 "github.com/satchel/satchel/pkg/api/v1"
)

// Object 是 settings show / set / rollback / master-url set 的输出：SystemSettings 的资源信封。
type Object = v1.Object[v1.SystemSettingsSpec, v1.SystemSettingsStatus]

// RevealedObject 是带 secrets scope 的身份拿到的输出：形状与 Object 相同，打码字段是原文（master-api-tokens「密钥读取开关」）。
type RevealedObject = v1.Object[map[string]any, map[string]any]

// Service 持有仓储，以及设置写成功之后要通知的回调（装配根登记：门、登录限流、封禁与 Turnstile 按新值生效）。
type Service struct {
	repo    *core.Repo
	onWrite []func(*core.State)
}

// New 建服务。
func New(repo *core.Repo) *Service {
	return &Service{repo: repo}
}

// Bindings 是本服务提供的命令处理函数，按命令名给 cmd/satchel 绑定。
func (s *Service) Bindings() command.Bindings {
	return command.Bindings{
		"settings show":           s.show,
		"settings set":            s.set,
		"settings snapshots list": s.snapshotsList,
		"settings rollback":       s.rollback,
		"settings master-url set": s.masterURLSet,
		"settings gates set":      s.gatesSet,
	}
}

// OnWrite 登记一个回调：四条写命令（set、rollback、master-url set、gates set）每次写成功之后，
// 把写后的整份设置交给它。装配根在启动时登记，之后不再改。
func (s *Service) OnWrite(fn func(*core.State)) {
	s.onWrite = append(s.onWrite, fn)
}

func (s *Service) written(st *core.State) {
	for _, fn := range s.onWrite {
		fn(st)
	}
}

// admits 决定一条写命令能写哪些字段：不能写时返回 field_not_applyable（design 第 1 条：命令按分档拆，别的档一律拒绝）。
type admits func(f core.Field, what string) error

// byClass 按分档放行。
func byClass(allowed map[schema.Class]bool) admits {
	return func(f core.Field, what string) error {
		if !allowed[f.Class] {
			return v1.Newf(v1.CodeFieldNotApplyable, "字段 %s 是系统设置的%s字段，不能经 %s 写入", f.Name, classLabels[f.Class], what)
		}
		return nil
	}
}

var (
	specOnly  = byClass(map[schema.Class]bool{schema.ClassSpec: true})
	humanOnly = byClass(map[schema.Class]bool{schema.ClassHuman: true})
)

// gateFields 是第 05 章七组「门」这一组的 15 个字段，只有 settings gates set 能写（master-settings「门的设置是人类专属的设置写」）：
// 三道门的 4 个、登录限流与令牌猜测封禁的 8 个参数、Turnstile 的两个 key、反代登记。
var gateFields = map[string]bool{
	"master_local_only": true, "silent_mode": true, "silent_mode_timeout": true, "probe_disguise_block_login": true,
	"brute_force_enabled": true, "brute_force_max_failures": true, "brute_force_window_minutes": true, "brute_force_block_minutes": true,
	"login_rate_max_attempts": true, "login_rate_window_minutes": true, "login_rate_lock_minutes": true, "skip_local_ip": true,
	"turnstile_site_key": true, "turnstile_secret_key": true, "trusted_proxies": true,
}

// humanWriters 是七组别的组已有的专门写命令（还没有写命令的组不在这里）：门的写命令拒绝它们时指个路。
var humanWriters = map[string]string{"master_url": "settings master-url set", "subscription_url": "settings master-url set"}

// gatesOnly 只放行门这一组的字段。
func gatesOnly(f core.Field, what string) error {
	if gateFields[f.Name] {
		return nil
	}
	e := v1.Newf(v1.CodeFieldNotApplyable, "字段 %s 是系统设置的%s字段，不属于门这一组，不能经 %s 写入", f.Name, classLabels[f.Class], what)
	if cmd, ok := humanWriters[f.Name]; ok {
		e = e.WithNext("改它用 satchel " + cmd)
	}
	return e
}

var classLabels = map[schema.Class]string{
	schema.ClassSpec: "日常运维", schema.ClassHuman: "人类专属", schema.ClassMasterSelf: "主控自身类",
	schema.ClassReadOnly: "只读", schema.ClassStatus: "运行态", schema.ClassMeta: "元数据", schema.ClassAction: "动作专属",
}

// requireAdmin：settings * 只对管理员身份开放（本机管理员与管理员账号），M3 的权限配额再细分。
func requireAdmin(ctx context.Context) error {
	if !v1.IdentityFrom(ctx).IsAdmin() {
		return v1.New(v1.CodeForbidden, "系统设置只对管理员开放")
	}
	return nil
}

// output 按身份决定打码：带 secrets scope 的身份（本机管理员、管理员账号的会话、打开了密钥读取的令牌）拿原文，
// spec / status 按字段分档直接从字段值表编；其余身份经生成的结构体，打码字段输出 ***。
func (s *Service) output(ctx context.Context, st *core.State) (any, error) {
	if v1.IdentityFrom(ctx).HasScope(v1.ScopeSecrets) {
		return s.revealed(st), nil
	}
	return s.object(st)
}

// revealed 按分档把字段值表分成 spec（日常运维）与 status（人类专属、主控自身类、只读、运行态），与生成的结构体同一套字段。
func (s *Service) revealed(st *core.State) *RevealedObject {
	spec, status := map[string]any{}, map[string]any{}
	for _, f := range s.repo.Fields() {
		switch f.Class {
		case schema.ClassSpec:
			spec[f.Name] = st.Values[f.Name]
		case schema.ClassHuman, schema.ClassMasterSelf, schema.ClassReadOnly, schema.ClassStatus:
			status[f.Name] = st.Values[f.Name]
		}
	}
	return &RevealedObject{
		APIVersion: v1.APIVersion, Kind: v1.Kind(core.KindName),
		Metadata: v1.Metadata{ID: core.SingletonID, ResourceVersion: st.Version, CreatedAt: st.CreatedAt, UpdatedAt: st.UpdatedAt},
		Spec:     spec, Status: status,
	}
}

// object 把字段值表套成资源信封：经 JSON 往返进生成的 Spec / Status 结构体，打码由 Secret 类型完成，
// 各自只认自己那一档的字段（多余的键被忽略）。
func (s *Service) object(st *core.State) (*Object, error) {
	raw, err := json.Marshal(st.Values)
	if err != nil {
		return nil, v1.Wrap(v1.CodeInternal, "编码系统设置失败", err)
	}
	var spec v1.SystemSettingsSpec
	var status v1.SystemSettingsStatus
	if err := json.Unmarshal(raw, &spec); err != nil {
		return nil, v1.Wrap(v1.CodeInternal, "系统设置的 spec 字段类型对不上", err)
	}
	if err := json.Unmarshal(raw, &status); err != nil {
		return nil, v1.Wrap(v1.CodeInternal, "系统设置的 status 字段类型对不上", err)
	}
	return &Object{
		APIVersion: v1.APIVersion, Kind: v1.Kind(core.KindName),
		Metadata: v1.Metadata{ID: core.SingletonID, ResourceVersion: st.Version, CreatedAt: st.CreatedAt, UpdatedAt: st.UpdatedAt},
		Spec:     spec, Status: status,
	}, nil
}

func (s *Service) show(ctx context.Context, _ *command.Invocation) (any, error) {
	if err := requireAdmin(ctx); err != nil {
		return nil, err
	}
	st, err := s.repo.Load(ctx)
	if err != nil {
		return nil, err
	}
	return s.output(ctx, st)
}

// versionArgs 取 --resource-version：没给是 bad_request（整单一个版本，改任何一档都要带）。没有跳过比对的写法：
// 冲突了就重新读一遍再改（第 07 章），force 归危险操作的权限类，随 M2 的 apply 一起做门。
func versionArgs(inv *command.Invocation) (int64, error) {
	raw, given := inv.Flags["resource-version"]
	if !given {
		return 0, v1.New(v1.CodeBadRequest, "要带 --resource-version（settings show 里 metadata.resourceVersion 的值）").
			WithNext("先运行 satchel settings show --json 看当前版本")
	}
	switch n := raw.(type) {
	case int:
		return int64(n), nil
	case int64:
		return n, nil
	case json.Number:
		i, err := n.Int64()
		if err != nil {
			return 0, v1.Newf(v1.CodeBadRequest, "参数 resource-version 的值 %s 不是整数", n)
		}
		return i, nil
	}
	return 0, v1.Newf(v1.CodeBadRequest, "参数 resource-version 必须是整数，得到 %T", raw)
}

// prepare 对一次写的字段做整体校验（任一字段不过整单拒绝、不部分写入）：字段存在、分档在允许集合里、值按类型归一、过字段规则；
// 打码字段收到 *** 表示保持不变、从本次写里剔除。what 是命令名，进错误文案。
func (s *Service) prepare(values map[string]any, admit admits, what string) (map[string]any, error) {
	if len(values) == 0 {
		return nil, v1.Newf(v1.CodeBadRequest, "%s 没有要改的字段", what).WithNext("用 --set 字段=值 给出要改的字段，字段清单见 satchel explain SystemSettings")
	}
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make(map[string]any, len(values))
	for _, name := range names {
		f, ok := s.repo.Field(name)
		if !ok {
			return nil, v1.Newf(v1.CodeUnknownField, "系统设置没有字段 %s", name).WithNext("字段清单见 satchel explain SystemSettings")
		}
		if err := admit(f, what); err != nil {
			return nil, err
		}
		v := values[name]
		if f.Masked {
			if str, ok := v.(string); ok && str == v1.Redacted {
				continue
			}
		}
		typed, err := core.Normalize(f.Type, v)
		if err != nil {
			return nil, v1.Newf(v1.CodeBadRequest, "字段 %s：%s", name, v1.AsError(err).Reason)
		}
		if rule, ok := rules[name]; ok {
			if err := rule(typed); err != nil {
				return nil, v1.Newf(v1.CodeBadRequest, "字段 %s %s", name, err)
			}
		}
		out[name] = typed
	}
	if len(out) == 0 {
		return nil, v1.Newf(v1.CodeBadRequest, "%s 没有要改的字段：打码字段的 %s 表示保持不变", what, v1.Redacted)
	}
	return out, nil
}

func (s *Service) set(ctx context.Context, inv *command.Invocation) (any, error) {
	if err := requireAdmin(ctx); err != nil {
		return nil, err
	}
	expected, err := versionArgs(inv)
	if err != nil {
		return nil, err
	}
	values, _ := inv.Flags["set"].(map[string]any)
	prepared, err := s.prepare(values, specOnly, inv.Name())
	if err != nil {
		return nil, err
	}
	st, err := s.repo.Write(ctx, core.WriteRequest{Values: prepared, ExpectedVersion: expected, Snapshot: true, Source: core.SourceSettings})
	if err != nil {
		return nil, err
	}
	s.written(st)
	return s.output(ctx, st)
}

func (s *Service) snapshotsList(ctx context.Context, inv *command.Invocation) (any, error) {
	if err := requireAdmin(ctx); err != nil {
		return nil, err
	}
	page := command.Page{}
	if inv.Page != nil {
		page = *inv.Page
	}
	if err := page.Normalize(); err != nil {
		return nil, err
	}
	beforeID, err := command.DecodeIDCursor(page.Cursor)
	if err != nil {
		return nil, err
	}
	total, err := s.repo.CountSnapshots(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := s.repo.ListSnapshots(ctx, page.Limit+1, beforeID)
	if err != nil {
		return nil, err
	}
	res := &command.PageResult{Items: make([]any, 0, len(rows)), Total: total}
	if len(rows) > page.Limit {
		rows = rows[:page.Limit]
		res.NextCursor = command.EncodeIDCursor(rows[len(rows)-1].ID)
	}
	for _, r := range rows {
		res.Items = append(res.Items, r)
	}
	return res, nil
}

func (s *Service) rollback(ctx context.Context, inv *command.Invocation) (any, error) {
	if err := requireAdmin(ctx); err != nil {
		return nil, err
	}
	id, err := strconv.ParseInt(inv.Arg(0), 10, 64)
	if err != nil || id <= 0 {
		return nil, v1.Newf(v1.CodeBadRequest, "快照 id 必须是正整数，得到 %q", inv.Arg(0)).WithNext("用 settings snapshots list 查看可用的快照")
	}
	expected, err := versionArgs(inv)
	if err != nil {
		return nil, err
	}
	snap, err := s.repo.GetSnapshot(ctx, id)
	if err != nil {
		return nil, err
	}
	values, err := s.valuesFromSnapshot(snap.Content)
	if err != nil {
		return nil, err
	}
	// 快照内容当成一次 settings set 写回：重过分档与规则（功能⑨「写回时重新过一遍字段分档检查」）。
	prepared, err := s.prepare(values, specOnly, inv.Name())
	if err != nil {
		return nil, err
	}
	st, err := s.repo.Write(ctx, core.WriteRequest{Values: prepared, ExpectedVersion: expected, Snapshot: true, Source: core.SourceRollback})
	if err != nil {
		return nil, err
	}
	s.written(st)
	return s.output(ctx, st)
}

// valuesFromSnapshot 把快照内容解成 prepare 能收的值：json 类型的字段直接给原始 JSON（值本身可能是一个 JSON 字符串，
// 不能先解成 Go 字符串再当 JSON 文本解析），其它字段按 JSON 原生类型解（整数用 json.Number，不经 float64）。
func (s *Service) valuesFromSnapshot(content json.RawMessage) (map[string]any, error) {
	var byKey map[string]json.RawMessage
	if err := json.Unmarshal(content, &byKey); err != nil {
		return nil, v1.Wrap(v1.CodeInternal, "快照内容不是 JSON 对象", err)
	}
	values := make(map[string]any, len(byKey))
	for name, raw := range byKey {
		if f, ok := s.repo.Field(name); ok && f.Type == schema.TypeJSON {
			values[name] = raw
			continue
		}
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.UseNumber()
		var v any
		if err := dec.Decode(&v); err != nil {
			return nil, v1.Wrap(v1.CodeInternal, "快照内容里字段 "+name+" 不是合法的 JSON", err)
		}
		values[name] = v
	}
	return values, nil
}

// masterURLSet 改七组的主控地址与订阅域名：人类专属（当场验证由 authz 在这之前做），同一事务、只抬版本、不存快照。
// 改完不推送到节点、不做主控迁移（M2）。
func (s *Service) masterURLSet(ctx context.Context, inv *command.Invocation) (any, error) {
	if err := requireAdmin(ctx); err != nil {
		return nil, err
	}
	expected, err := versionArgs(inv)
	if err != nil {
		return nil, err
	}
	values := map[string]any{}
	for flag, field := range map[string]string{"url": "master_url", "subscription-url": "subscription_url"} {
		raw, given := inv.Flags[flag]
		if !given {
			continue
		}
		str, _ := raw.(string)
		origin, err := normalizeOrigin(flag, str)
		if err != nil {
			return nil, err
		}
		values[field] = origin
	}
	if len(values) == 0 {
		return nil, v1.New(v1.CodeBadRequest, "至少要给 --url 或 --subscription-url 之一（空串表示清掉）")
	}
	prepared, err := s.prepare(values, humanOnly, inv.Name())
	if err != nil {
		return nil, err
	}
	st, err := s.repo.Write(ctx, core.WriteRequest{Values: prepared, ExpectedVersion: expected})
	if err != nil {
		return nil, err
	}
	s.written(st)
	return s.output(ctx, st)
}

// gatesSet 改七组「门」这一组的字段：人类专属（当场验证由 authz 在这之前做），整体校验、同一事务、只抬版本、不存快照
// （七组字段不进快照）。写完经回调通知门、登录限流、封禁与 Turnstile，下一个请求就按新值判定。
func (s *Service) gatesSet(ctx context.Context, inv *command.Invocation) (any, error) {
	if err := requireAdmin(ctx); err != nil {
		return nil, err
	}
	expected, err := versionArgs(inv)
	if err != nil {
		return nil, err
	}
	values, _ := inv.Flags["set"].(map[string]any)
	prepared, err := s.prepare(values, gatesOnly, inv.Name())
	if err != nil {
		return nil, err
	}
	st, err := s.repo.Write(ctx, core.WriteRequest{Values: prepared, ExpectedVersion: expected})
	if err != nil {
		return nil, err
	}
	s.written(st)
	return s.output(ctx, st)
}
