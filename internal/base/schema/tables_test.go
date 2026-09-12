package schema

import (
	"strconv"
	"strings"
	"testing"
)

// 表名清单，按外键拓扑顺序；加表要同时改这里。
const goldenTables = "audit_logs,automation_rules,batch_op_counters,config_snapshots,evidence_packages,alerts,jobs,notify_channels,notify_deliveries,alert_deliveries,plans,tasks"

func TestDefaultTables(t *testing.T) {
	r := Default()
	var names []string
	for _, tbl := range r.Tables() {
		names = append(names, tbl.Name)
	}
	if got := strings.Join(names, ","); got != goldenTables {
		t.Fatalf("表名清单变了：\n得到 %s\n想要 %s", got, goldenTables)
	}
}

func TestKindTablesCarryVersionColumnsByClass(t *testing.T) {
	for _, tbl := range Default().Tables() {
		_, hasDeleted := tbl.Column("deleted_at")
		switch tbl.KindClass {
		case KindConfig, KindAction:
			if !tbl.HasVersion() || !hasDeleted {
				t.Errorf("表 %s（%s）应当有 resource_version 与 deleted_at", tbl.Name, tbl.KindClass)
			}
		case KindMasterSettings:
			if !tbl.HasVersion() || hasDeleted {
				t.Errorf("表 %s（主控设置类）应当只有 resource_version", tbl.Name)
			}
		default:
			if tbl.HasVersion() || hasDeleted {
				t.Errorf("表 %s（%s）不该有 resource_version 或 deleted_at", tbl.Name, tbl.KindClass)
			}
		}
	}
}

func TestAppendOnlyTables(t *testing.T) {
	want := map[string]bool{"audit_logs": true, "config_snapshots": true, "batch_op_counters": true}
	for _, tbl := range Default().Tables() {
		if tbl.AppendOnly != want[tbl.Name] {
			t.Errorf("表 %s 的 append-only = %v，想要 %v", tbl.Name, tbl.AppendOnly, want[tbl.Name])
		}
	}
}

func TestEveryKindHasClass(t *testing.T) {
	for _, tbl := range Default().Tables() {
		if tbl.IsKind() && tbl.KindClass == KindNone {
			t.Errorf("kind %s 没有类别", tbl.Kind)
		}
	}
}

func TestNaturalKeysAndChecks(t *testing.T) {
	r := Default()
	// 只有用户起的名字是自然键；job_id、delivery_id 是系统生成的幂等 id，撞上要报 conflict 而不是 name_taken。
	wantNatural := map[string]string{
		"automation_rules": "automation_rules_name_key",
		"notify_channels":  "notify_channels_name_key",
	}
	for _, tbl := range r.Tables() {
		for _, ix := range tbl.Indexes {
			if ix.NaturalKey != (wantNatural[tbl.Name] == ix.Name) {
				t.Errorf("索引 %s 的 NaturalKey = %v，与清单不符", ix.Name, ix.NaturalKey)
			}
		}
	}
	alerts, _ := r.Table("alerts")
	if c, _ := alerts.Column("dedup_key"); c.Check != "dedup_key <> ''" {
		t.Errorf("alerts.dedup_key 应当有非空 CHECK，得到 %q", c.Check)
	}
	channels, _ := r.Table("notify_channels")
	if c, _ := channels.Column("enabled"); c.Default != "FALSE" {
		t.Errorf("notify_channels.enabled 默认值应当是 FALSE，得到 %q", c.Default)
	}
}

// 注册表里每个标了打码的列都要出现在生成的 kind 清单里（对照生成物）。
func TestMaskedColumnsAppearInGeneratedKinds(t *testing.T) {
	src, err := GenerateKinds(Default())
	if err != nil {
		t.Fatal(err)
	}
	text := squash(string(src))
	for _, tbl := range Default().Tables() {
		if !tbl.IsKind() {
			continue
		}
		for _, c := range tbl.Columns {
			if !c.Masked {
				continue
			}
			want := "Name: " + strconv.Quote(tbl.Kind)
			start := strings.Index(text, want)
			if start < 0 {
				t.Fatalf("生成物里没有 kind %s", tbl.Kind)
			}
			block := text[start:]
			if end := strings.Index(block, "}, {"); end > 0 {
				block = block[:end]
			}
			const marker = "MaskedFields: []string{"
			i := strings.Index(block, marker)
			if i < 0 {
				t.Fatalf("kind %s 的清单里没有 MaskedFields", tbl.Kind)
			}
			masked := block[i+len(marker):]
			masked = masked[:strings.Index(masked, "}")]
			if !strings.Contains(masked, strconv.Quote(c.Name)) {
				t.Errorf("kind %s 的打码列 %s 不在生成的 MaskedFields 里，得到 {%s}", tbl.Kind, c.Name, masked)
			}
		}
	}
}
