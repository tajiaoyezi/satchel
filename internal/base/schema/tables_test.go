package schema

import (
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
