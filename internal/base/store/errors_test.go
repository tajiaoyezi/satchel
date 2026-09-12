package store

import (
	"strings"
	"testing"

	"github.com/satchel/satchel/internal/base/schema"
)

func TestColumnsFromSQLiteDetail(t *testing.T) {
	cases := map[string]string{
		"constraint failed: UNIQUE constraint failed: automation_rules.name (2067)":                                   "name",
		"constraint failed: UNIQUE constraint failed: alert_deliveries.alert_id, alert_deliveries.delivery_id (1555)": "alert_id,delivery_id",
		"UNIQUE constraint failed: tasks.dedup_key":                                                                   "dedup_key",
		"constraint failed: NOT NULL constraint failed: alerts.category (1299)":                                       "category",
		"constraint failed: FOREIGN KEY constraint failed (787)":                                                      "",
	}
	for in, want := range cases {
		if got := strings.Join(columnsFromDetail(sqliteDetail(in)), ","); got != want {
			t.Errorf("columnsFromDetail(%q) = %q，想要 %q", in, got, want)
		}
	}
}

func TestColumnByCheckAndPGConstraintColumn(t *testing.T) {
	alerts, _ := schema.Default().Table("alerts")
	if got := columnByCheck(alerts, sqliteDetail("constraint failed: CHECK constraint failed: dedup_key <> '' (275)")); got != "dedup_key" {
		t.Errorf("按谓词找列得到 %q，想要 dedup_key", got)
	}
	if got := columnByCheck(alerts, sqliteDetail("CHECK constraint failed: status IN ('open', 'claimed', 'recovered', 'resolved') (275)")); got != "status" {
		t.Errorf("按枚举谓词找列得到 %q，想要 status", got)
	}
	if got := columnByCheck(alerts, "nope"); got != "" {
		t.Errorf("对不上的谓词应当返回空，得到 %q", got)
	}
	for in, want := range map[string]string{"alerts_dedup_key_check": "dedup_key", "alerts_status_check1": "status", "alerts_nope_check": "", "tasks_alert_id_fkey": ""} {
		if got := pgConstraintColumn(alerts, in); got != want {
			t.Errorf("pgConstraintColumn(alerts, %q) = %q，想要 %q", in, got, want)
		}
	}
	tasks, _ := schema.Default().Table("tasks")
	if got := pgConstraintColumn(tasks, "tasks_alert_id_fkey"); got != "alert_id" {
		t.Errorf("外键约束名应当解出 alert_id，得到 %q", got)
	}
}
