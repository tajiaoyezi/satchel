package tunnels

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/uptrace/bun"

	"github.com/satchel/satchel/internal/base/db/dbtest"
	"github.com/satchel/satchel/internal/base/nginx"
	"github.com/satchel/satchel/internal/base/schema"
	"github.com/satchel/satchel/internal/base/singbox"
	"github.com/satchel/satchel/internal/base/store"
	"github.com/satchel/satchel/internal/command"
	coreinbounds "github.com/satchel/satchel/internal/core/inbounds"
	corerouting "github.com/satchel/satchel/internal/core/routingrules"
	coreservers "github.com/satchel/satchel/internal/core/servers"
	coresettings "github.com/satchel/satchel/internal/core/settings"
	corewebsites "github.com/satchel/satchel/internal/core/websites"
	v1 "github.com/satchel/satchel/pkg/api/v1"
)

type recordedOp struct {
	Op              string          `json:"op"`
	Kind            string          `json:"kind"`
	ID              int64           `json:"id,omitempty"`
	ResourceVersion int64           `json:"resourceVersion,omitempty"`
	Spec            json.RawMessage `json:"spec,omitempty"`
}

type fakePlanner struct {
	lastJSON string
	lastOps  []recordedOp
}

func (f *fakePlanner) CreatePlanFromJSON(_ context.Context, specJSON, _, _ string) (any, error) {
	f.lastJSON = specJSON
	var ops []recordedOp
	if err := json.Unmarshal([]byte(specJSON), &ops); err != nil {
		return nil, err
	}
	f.lastOps = ops
	return map[string]any{"ops": ops, "count": len(ops)}, nil
}

func TestStealStatusAndPlanModes(t *testing.T) {
	dbtest.ForEach(t, func(t *testing.T, bdb *bun.DB) {
		ctx := context.Background()
		st := store.New(bdb, schema.Default())
		setRepo := coresettings.New(bdb, st, schema.Default())
		if err := setRepo.EnsureSingleton(ctx); err != nil {
			t.Fatal(err)
		}
		srvRepo := coreservers.New(bdb, st)
		inRepo := coreinbounds.New(bdb, st)
		rrRepo := corerouting.New(bdb, st)
		wsRepo := corewebsites.New(bdb, st)
		planner := &fakePlanner{}

		svc := New(Deps{
			Servers:      srvRepo,
			Inbounds:     inRepo,
			RoutingRules: rrRepo,
			Websites:     wsRepo,
			Settings:     setRepo,
			Planner:      planner,
		})
		admin := v1.WithIdentity(ctx, v1.LocalAdmin("root"))
		user := v1.WithIdentity(ctx, v1.Identity{Actor: "bob", ActorKind: v1.ActorUser, Role: v1.RoleUser, Scopes: []v1.Scope{v1.ScopeRead}})
		b := svc.Bindings()

		// 1. 普通用户全部 9 条命令均返回 forbidden。
		for name, fn := range b {
			if _, err := fn(user, &command.Invocation{Args: []string{"hk-1"}}); err == nil || v1.AsError(err).Code != v1.CodeForbidden {
				t.Errorf("普通用户调用 %s 应报 forbidden，实际：%v", name, err)
			}
		}

		// 准备远程节点 hk-1（带 1 条 Reality 入站 + 1 条 WS 入站）和同机节点 local-1。
		hk1, err := srvRepo.Create(ctx, "hk-1", coreservers.ModeAuto)
		if err != nil {
			t.Fatal(err)
		}
		local1, err := srvRepo.Create(ctx, "local-1", coreservers.ModeAuto)
		if err != nil {
			t.Fatal(err)
		}
		if err := srvRepo.SetSameHost(ctx, local1.Metadata.ID, true); err != nil {
			t.Fatal(err)
		}

		var realityIB, wsIB *coreinbounds.Inbound
		if err := bdb.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
			var err error
			realityIB, err = inRepo.CreateTx(ctx, tx, v1.InboundSpec{
				ServerID: hk1.Metadata.ID,
				Tag:      "vless-real",
				Protocol: "vless",
				Port:     18443,
				Enabled:  true,
				TLS:      v1.SecretJSON(`{"enabled":true,"server_name":"www.microsoft.com","reality":{"enabled":true,"handshake":{"server":"www.microsoft.com","server_port":443},"private_key":"priv-1","short_id":["a1b2"]}}`),
				Settings: v1.SecretJSON(`{"users":[{"name":"u1","uuid":"6a4f1c2e-3b5d-4e7f-8a9b-0c1d2e3f4a5b"}]}`),
			})
			if err != nil {
				return err
			}
			wsIB, err = inRepo.CreateTx(ctx, tx, v1.InboundSpec{
				ServerID:  hk1.Metadata.ID,
				Tag:       "vless-ws",
				Protocol:  "vless",
				Port:      20001,
				Listen:    "127.0.0.1",
				Enabled:   true,
				Transport: json.RawMessage(`{"type":"ws","path":"/ray"}`),
				Settings:  v1.SecretJSON(`{"users":[{"name":"u1","uuid":"6a4f1c2e-3b5d-4e7f-8a9b-0c1d2e3f4a5b"}]}`),
			})
			return err
		}); err != nil {
			t.Fatal(err)
		}

		// 2. 同机节点拒绝开启偷取模式。
		if _, err := b["server steal plan"](admin, &command.Invocation{
			Args:  []string{"local-1"},
			Flags: map[string]any{"mode": "tunnel", "domain": "decoy.example.com"},
		}); err == nil || v1.AsError(err).Code != v1.CodeConflict {
			t.Fatalf("同机节点开启 steal 应报 conflict，实际：%v", err)
		}

		// 3. 与同机已有 Website 域名冲突报 conflict。
		if err := bdb.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
			_, err := wsRepo.CreateTx(ctx, tx, v1.WebsiteSpec{
				ServerID: hk1.Metadata.ID, Domain: "taken.example.com", Type: nginx.TypeStatic,
			})
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := b["server steal plan"](admin, &command.Invocation{
			Args:  []string{"hk-1"},
			Flags: map[string]any{"mode": "tunnel", "domain": "taken.example.com"},
		}); err == nil || v1.AsError(err).Code != v1.CodeConflict {
			t.Fatalf("与同机 Website 撞域名应报 conflict，实际：%v", err)
		}

		// 4. tunnel 模式规划：创建 tunnel-in + 2 条规则 + 重映射 Reality 到 127.0.0.1:18443，WS 入站完全不动。
		if _, err := b["server steal plan"](admin, &command.Invocation{
			Args:  []string{"hk-1"},
			Flags: map[string]any{"mode": "tunnel", "domain": "decoy.example.com"},
		}); err != nil {
			t.Fatalf("tunnel 模式 plan 失败：%v", err)
		}
		if len(planner.lastOps) != 5 {
			t.Fatalf("tunnel 模式期望 5 个变更项（Server + tunnel-in + 2 rules + reality update），得到 %d：%s", len(planner.lastOps), planner.lastJSON)
		}
		if strings.Contains(planner.lastJSON, "vless-ws") || strings.Contains(planner.lastJSON, `"id":`+jsonInt(wsIB.Metadata.ID)) {
			t.Fatalf("WS 入站不应被修改：%s", planner.lastJSON)
		}
		if !strings.Contains(planner.lastJSON, `"tag":"tunnel-in"`) || !strings.Contains(planner.lastJSON, `"network":"tcp"`) ||
			!strings.Contains(planner.lastJSON, `"override_port":8001`) || !strings.Contains(planner.lastJSON, `"override_port":18443`) {
			t.Fatalf("tunnel 模式 ops 缺少关键字段：%s", planner.lastJSON)
		}

		// 模拟 tunnel-in 曾经创建并软删除 -> 再次 --mode tunnel 必须生成 op:"restore" 按原 ID 恢复！
		var tunnelInID int64
		if err := bdb.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
			created, err := inRepo.CreateTx(ctx, tx, v1.InboundSpec{
				ServerID: hk1.Metadata.ID, Tag: singbox.StealTunnelInboundTag, Protocol: "direct", Port: 443,
				Settings: v1.SecretJSON(`{"network":"tcp"}`), Enabled: true,
			})
			if err != nil {
				return err
			}
			tunnelInID = created.Metadata.ID
			_, err = inRepo.SoftDeleteTx(ctx, tx, created.Metadata.ID, created.Metadata.ResourceVersion, false)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := b["server steal plan"](admin, &command.Invocation{
			Args:  []string{"hk-1"},
			Flags: map[string]any{"mode": "tunnel", "domain": "decoy.example.com"},
		}); err != nil {
			t.Fatalf("软删后恢复 tunnel-in plan 失败：%v", err)
		}
		foundRestore := false
		for _, op := range planner.lastOps {
			if op.Op == "restore" && op.Kind == coreinbounds.KindName && op.ID == tunnelInID {
				foundRestore = true
			}
		}
		if !foundRestore {
			t.Fatalf("tunnel-in 软删后再次 tunnel plan 必须使用 op=restore，实际：%s", planner.lastJSON)
		}

		// 5. fallback 模式：重映射 Reality 至对外 443 + handshake 127.0.0.1:8001。
		if _, err := b["server steal plan"](admin, &command.Invocation{
			Args:  []string{"hk-1"},
			Flags: map[string]any{"mode": "fallback", "domain": "decoy.example.com"},
		}); err != nil {
			t.Fatalf("fallback 模式 plan 失败：%v", err)
		}
		foundReal443 := false
		for _, op := range planner.lastOps {
			if op.Op == "update" && op.Kind == coreinbounds.KindName && op.ID == realityIB.Metadata.ID {
				if strings.Contains(string(op.Spec), `"port":443`) && strings.Contains(string(op.Spec), `"server_port":8001`) {
					foundReal443 = true
				}
			}
		}
		if !foundReal443 {
			t.Fatalf("fallback 模式未将 Reality 重映射至 443 + 8001：%s", planner.lastJSON)
		}

		// 6. server steal status 在开启 use_443 后返回偷自己预览（不含 proxy_protocol）与 next_free_port。
		updSpec := hk1.Spec
		updSpec.Use443 = true
		updSpec.StealMode = "fallback"
		updSpec.Domain = "decoy.example.com"
		if err := bdb.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
			_, err := srvRepo.UpdateSpecTx(ctx, tx, hk1.Metadata.ID, hk1.Metadata.ResourceVersion, false, updSpec)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		stAny, err := b["server steal status"](admin, &command.Invocation{Args: []string{"hk-1"}})
		if err != nil {
			t.Fatalf("server steal status 失败：%v", err)
		}
		sv := stAny.(*StealStatusView)
		if !sv.Use443 || sv.NextFreePort <= 0 || sv.StealNginxPreview == nil ||
			!strings.Contains(sv.StealNginxPreview.Conf, "location /ray {") ||
			strings.Contains(sv.StealNginxPreview.Conf, "proxy_protocol") {
			t.Fatalf("server steal status 输出异常：%+v", sv)
		}
	})
}

func TestSingleHopMultiplexAndChainTunnels(t *testing.T) {
	dbtest.ForEach(t, func(t *testing.T, bdb *bun.DB) {
		ctx := context.Background()
		st := store.New(bdb, schema.Default())
		srvRepo := coreservers.New(bdb, st)
		inRepo := coreinbounds.New(bdb, st)
		rrRepo := corerouting.New(bdb, st)
		planner := &fakePlanner{}
		svc := New(Deps{
			Servers:      srvRepo,
			Inbounds:     inRepo,
			RoutingRules: rrRepo,
			Planner:      planner,
		})
		admin := v1.WithIdentity(ctx, v1.LocalAdmin("root"))
		b := svc.Bindings()

		// 建三台服务器 s1 / s2 / s3，给 s2 / s3 配可达地址。
		s1, _ := srvRepo.Create(ctx, "s1", coreservers.ModeAuto)
		s2, _ := srvRepo.Create(ctx, "s2", coreservers.ModeAuto)
		s3, _ := srvRepo.Create(ctx, "s3", coreservers.ModeAuto)
		now := time.Now().UTC()
		_ = srvRepo.MarkOnline(ctx, s1.Metadata.ID, "10.0.0.1", false, now, now)
		_ = srvRepo.MarkOnline(ctx, s2.Metadata.ID, "10.0.0.2", false, now, now)
		_ = srvRepo.MarkOnline(ctx, s3.Metadata.ID, "10.0.0.3", false, now, now)

		// 在 s2 上建一条 hysteria2（UDP）入站，测试 --target-server + --target-inbound。
		if err := bdb.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
			_, err := inRepo.CreateTx(ctx, tx, v1.InboundSpec{
				ServerID: s2.Metadata.ID, Tag: "hy2-in", Protocol: "hysteria2", Port: 28443, Enabled: true,
				TLS: v1.SecretJSON(`{"enabled":true}`),
			})
			return err
		}); err != nil {
			t.Fatal(err)
		}

		// 1. tunnel plan 指向已有服务器入站 -> 自动解析 10.0.0.2:28443 与 udp 网络。
		if _, err := b["tunnel plan"](admin, &command.Invocation{Flags: map[string]any{
			"server-name":    "s1",
			"listen-port":    21001,
			"target-server":  "s2",
			"target-inbound": "hy2-in",
		}}); err != nil {
			t.Fatalf("tunnel plan 引用已有入站失败：%v", err)
		}
		if len(planner.lastOps) != 2 || !strings.Contains(planner.lastJSON, `"network":"udp"`) ||
			!strings.Contains(planner.lastJSON, `"override_address":"10.0.0.2"`) || !strings.Contains(planner.lastJSON, `"override_port":28443`) {
			t.Fatalf("tunnel plan 输出不对：%s", planner.lastJSON)
		}

		// 2. tunnel plan 双协议 tcp,udp 绝不写入非法的 "tcp,udp" 字符串。
		if _, err := b["tunnel plan"](admin, &command.Invocation{Flags: map[string]any{
			"server-name":    "s1",
			"listen-port":    21002,
			"network":        "tcp,udp",
			"target-address": "198.51.100.9",
			"target-port":    9000,
		}}); err != nil {
			t.Fatalf("tunnel plan 双协议失败：%v", err)
		}
		if strings.Contains(planner.lastJSON, `"tcp,udp"`) {
			t.Fatalf("双协议不能把 \"tcp,udp\" 写入 direct settings：%s", planner.lastJSON)
		}

		// 3. 自环拦截（同机同端口）。
		if _, err := b["tunnel plan"](admin, &command.Invocation{Flags: map[string]any{
			"server-name":    "s1",
			"listen-port":    21003,
			"target-address": "10.0.0.1",
			"target-port":    21003,
		}}); err == nil || v1.AsError(err).Code != v1.CodeBadRequest {
			t.Fatalf("自环应报 bad_request，实际：%v", err)
		}

		// 4. 在 s1 落库一条散装 direct 入站 + 兜底规则 + 一条复用规则 + 一条 tunnel-in + 链式跳，测试 multiplex plan / list / delete-plan。
		var directIB *coreinbounds.Inbound
		var catchAllRR, muxRR *corerouting.RoutingRule
		if err := bdb.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
			var err error
			directIB, err = inRepo.CreateTx(ctx, tx, v1.InboundSpec{
				ServerID: s1.Metadata.ID, Tag: "tunnel-22000", Protocol: "direct", Port: 22000, Enabled: true,
			})
			if err != nil {
				return err
			}
			catchAllRR, err = rrRepo.CreateTx(ctx, tx, v1.RoutingRuleSpec{
				ServerID: s1.Metadata.ID, SortOrder: 50, OutboundTag: "direct", Enabled: true,
				Match: json.RawMessage(`{"inbound":["tunnel-22000"],"override_address":"198.51.100.1","override_port":8080}`),
			})
			if err != nil {
				return err
			}
			muxRR, err = rrRepo.CreateTx(ctx, tx, v1.RoutingRuleSpec{
				ServerID: s1.Metadata.ID, SortOrder: 30, OutboundTag: "direct", Enabled: true,
				Match: json.RawMessage(`{"inbound":["tunnel-22000"],"domain":["b.example.com"],"override_address":"198.51.100.2","override_port":8443}`),
			})
			if err != nil {
				return err
			}
			// tunnel-in 应被 tunnel list 排除。
			_, err = inRepo.CreateTx(ctx, tx, v1.InboundSpec{
				ServerID: s1.Metadata.ID, Tag: singbox.StealTunnelInboundTag, Protocol: "direct", Port: 443,
				Settings: v1.SecretJSON(`{"network":"tcp"}`), Enabled: true,
			})
			return err
		}); err != nil {
			t.Fatal(err)
		}

		// multiplex plan 自动计算 sort_order = 50 - 10 = 40 < 50。
		if _, err := b["tunnel multiplex plan"](admin, &command.Invocation{Flags: map[string]any{
			"server-name":    "s1",
			"inbound-tag":    "tunnel-22000",
			"domain":         []string{"c.example.com"},
			"target-address": "198.51.100.3",
			"target-port":    9443,
		}}); err != nil {
			t.Fatalf("tunnel multiplex plan 失败：%v", err)
		}
		if len(planner.lastOps) != 1 || !strings.Contains(planner.lastJSON, `"sort_order":40`) {
			t.Fatalf("multiplex plan 期望 sort_order=40，得到：%s", planner.lastJSON)
		}

		// tunnel list 列出 inbound + multiplex，排除 tunnel-in。
		listAny, err := b["tunnel list"](admin, &command.Invocation{})
		if err != nil {
			t.Fatalf("tunnel list 失败：%v", err)
		}
		page := listAny.(*command.PageResult)
		if page.Total != 2 {
			t.Fatalf("tunnel list 期望 2 条（1 inbound + 1 multiplex，排除 tunnel-in），得到 %d：%+v", page.Total, page.Items)
		}

		// tunnel delete-plan --rule-id 仅删单条复用规则；--server-name + --tag 连带删入站和全部绑定规则。
		if _, err := b["tunnel delete-plan"](admin, &command.Invocation{Flags: map[string]any{"rule-id": int(muxRR.Metadata.ID)}}); err != nil {
			t.Fatalf("delete-plan --rule-id 失败：%v", err)
		}
		if len(planner.lastOps) != 1 || planner.lastOps[0].Kind != corerouting.KindName || planner.lastOps[0].ID != muxRR.Metadata.ID {
			t.Fatalf("按 rule-id 删除应仅删 1 条规则：%s", planner.lastJSON)
		}
		if _, err := b["tunnel delete-plan"](admin, &command.Invocation{Flags: map[string]any{"server-name": "s1", "tag": "tunnel-22000"}}); err != nil {
			t.Fatalf("delete-plan --tag 失败：%v", err)
		}
		if len(planner.lastOps) != 3 || planner.lastOps[0].ID != directIB.Metadata.ID || catchAllRR.Metadata.ID == 0 {
			t.Fatalf("按 tag 删除应原子删 1 入站 + 2 绑定规则（共 3 项），得到 %d：%s", len(planner.lastOps), planner.lastJSON)
		}

		// 5. 链式隧道：拦截单机、逗号拼接未拆分、空白项、重复机、缺地址机；兼容带逗号服务器名；合法 3 机链生成单份 6 对象 Plan。
		if _, err := b["tunnel chain plan"](admin, &command.Invocation{Flags: map[string]any{
			"label": "bad-one", "servers": []string{"s1"}, "target-address": "198.51.100.9", "target-port": 443,
		}}); err == nil || v1.AsError(err).Code != v1.CodeBadRequest {
			t.Fatalf("少于 2 台服务器应报 bad_request，得到：%v", err)
		}
		if _, err := b["tunnel chain plan"](admin, &command.Invocation{Flags: map[string]any{
			"label": "bad-comma-one", "servers": []string{"s1,s2"}, "target-address": "198.51.100.9", "target-port": 443,
		}}); err == nil || v1.AsError(err).Code != v1.CodeBadRequest {
			t.Fatalf("单元素逗号串不应拆分为多台，应报 bad_request，得到：%v", err)
		}
		if _, err := b["tunnel chain plan"](admin, &command.Invocation{Flags: map[string]any{
			"label": "bad-comma-two", "servers": []string{"s1,s2", "s3"}, "target-address": "198.51.100.9", "target-port": 443,
		}}); err == nil || v1.AsError(err).Code != v1.CodeNotFound {
			t.Fatalf("包含逗号拼接串的元素应按原样查找并报 not_found，得到：%v", err)
		}
		if _, err := b["tunnel chain plan"](admin, &command.Invocation{Flags: map[string]any{
			"label": "bad-blank", "servers": []string{"s1", "  "}, "target-address": "198.51.100.9", "target-port": 443,
		}}); err == nil || v1.AsError(err).Code != v1.CodeBadRequest {
			t.Fatalf("包含空白项应报 bad_request，得到：%v", err)
		}
		if _, err := b["tunnel chain plan"](admin, &command.Invocation{Flags: map[string]any{
			"label": "bad-dup", "servers": []string{"s1", "s1"}, "target-address": "198.51.100.9", "target-port": 443,
		}}); err == nil || v1.AsError(err).Code != v1.CodeBadRequest {
			t.Fatalf("重复服务器应报 bad_request，得到：%v", err)
		}
		sNoAddr, _ := srvRepo.Create(ctx, "s-noaddr", coreservers.ModeAuto)
		if _, err := b["tunnel chain plan"](admin, &command.Invocation{Flags: map[string]any{
			"label": "bad-addr", "servers": []string{"s1", sNoAddr.Spec.Name}, "target-address": "198.51.100.9", "target-port": 443,
		}}); err == nil || v1.AsError(err).Code != v1.CodeBadRequest {
			t.Fatalf("下游缺地址应报 bad_request，得到：%v", err)
		}
		// 带逗号的服务器名按原样精确匹配。
		sComma, err := srvRepo.Create(ctx, "hk,1", coreservers.ModeAuto)
		if err != nil {
			t.Fatal(err)
		}
		if err := srvRepo.MarkOnline(ctx, sComma.Metadata.ID, "10.0.0.9", false, now, now); err != nil {
			t.Fatal(err)
		}
		if _, err := b["tunnel chain plan"](admin, &command.Invocation{Flags: map[string]any{
			"label": "comma-srv", "servers": []string{"s1", "hk,1"}, "target-address": "198.51.100.9", "target-port": 443,
		}}); err != nil {
			t.Fatalf("带逗号的服务器名应原样匹配成功，得到：%v", err)
		}
		if len(planner.lastOps) != 4 || !strings.Contains(planner.lastJSON, `"override_address":"10.0.0.9"`) {
			t.Fatalf("带逗号服务器链规划异常：%s", planner.lastJSON)
		}

		if _, err := b["tunnel chain plan"](admin, &command.Invocation{Flags: map[string]any{
			"label":          "hk-jp-us",
			"servers":        []string{"s1", "s2", "s3"},
			"entry-port":     25000,
			"target-address": "198.51.100.99",
			"target-port":    443,
		}}); err != nil {
			t.Fatalf("3 机链式隧道 plan 失败：%v", err)
		}
		if len(planner.lastOps) != 6 {
			t.Fatalf("3 机链应生成 6 个变更项，实际 %d：%s", len(planner.lastOps), planner.lastJSON)
		}
		for _, want := range []string{
			`"tag":"tunnel-hk-jp-us-h0"`, `"tag":"tunnel-hk-jp-us-h1"`, `"tag":"tunnel-hk-jp-us-h2"`,
			`"override_address":"10.0.0.2"`, `"override_address":"10.0.0.3"`, `"override_address":"198.51.100.99"`,
		} {
			if !strings.Contains(planner.lastJSON, want) {
				t.Errorf("3 机链 ops 缺少 %s：\n%s", want, planner.lastJSON)
			}
		}

		// 模拟 3 机链落库后检查 tunnel chain list（complete=true）、tunnel list（不含链跳）与 tunnel chain delete-plan（6 对象）。
		if err := bdb.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
			hops := []struct {
				srvID int64
				tag   string
				next  string
				port  int
			}{
				{s1.Metadata.ID, "tunnel-hk-jp-us-h0", "10.0.0.2", 25000},
				{s2.Metadata.ID, "tunnel-hk-jp-us-h1", "10.0.0.3", 25000},
				{s3.Metadata.ID, "tunnel-hk-jp-us-h2", "198.51.100.99", 443},
			}
			for _, h := range hops {
				if _, err := inRepo.CreateTx(ctx, tx, v1.InboundSpec{
					ServerID: h.srvID, Tag: h.tag, Protocol: "direct", Port: 25000, Enabled: true,
				}); err != nil {
					return err
				}
				m, _ := json.Marshal(map[string]any{"inbound": []string{h.tag}, "override_address": h.next, "override_port": h.port})
				if _, err := rrRepo.CreateTx(ctx, tx, v1.RoutingRuleSpec{
					ServerID: h.srvID, SortOrder: 50, Match: m, OutboundTag: "direct", Enabled: true,
				}); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}

		clAny, err := b["tunnel chain list"](admin, &command.Invocation{})
		if err != nil {
			t.Fatalf("tunnel chain list 失败：%v", err)
		}
		clPage := clAny.(*command.PageResult)
		if clPage.Total != 1 {
			t.Fatalf("期望 1 条链，得到 %d", clPage.Total)
		}
		cv := clPage.Items[0].(ChainView)
		if cv.Label != "hk-jp-us" || cv.HopCount != 3 || !cv.Complete || cv.FinalTarget != "198.51.100.99:443" {
			t.Fatalf("ChainView 异常：%+v", cv)
		}
		// 再次查 tunnel list 仍为 2 条（不包含链跳）。
		if tlAny, err := b["tunnel list"](admin, &command.Invocation{}); err != nil || tlAny.(*command.PageResult).Total != 2 {
			t.Fatalf("tunnel list 不应包含链式跳，实际：%v %+v", err, tlAny)
		}
		// 重建同名链应报 name_taken。
		if _, err := b["tunnel chain plan"](admin, &command.Invocation{Flags: map[string]any{
			"label": "hk-jp-us", "servers": []string{"s1", "s2"}, "target-address": "198.51.100.99", "target-port": 443,
		}}); err == nil || v1.AsError(err).Code != v1.CodeNameTaken {
			t.Fatalf("重名链应报 name_taken，得到：%v", err)
		}
		// 整链清理 plan 应包含 6 个 delete 变更。
		if _, err := b["tunnel chain delete-plan"](admin, &command.Invocation{Flags: map[string]any{"label": "hk-jp-us"}}); err != nil {
			t.Fatalf("tunnel chain delete-plan 失败：%v", err)
		}
		if len(planner.lastOps) != 6 {
			t.Fatalf("整链 delete-plan 期望 6 个 delete 项，得到 %d：%s", len(planner.lastOps), planner.lastJSON)
		}
	})
}

func jsonInt(n int64) string {
	b, _ := json.Marshal(n)
	return string(b)
}
