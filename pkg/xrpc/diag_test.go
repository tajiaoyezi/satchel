package xrpc

import (
	"encoding/json"
	"testing"
	"time"
)

func TestDiagItemsValidationAndJSON(t *testing.T) {
	if len(DiagItems) != 10 {
		t.Fatalf("期望 10 项固定诊断项，得到 %d", len(DiagItems))
	}
	for _, item := range DiagItems {
		if !ValidDiagItem(item) {
			t.Fatalf("合法诊断项 %q 被拒", item)
		}
		if !ValidDiagItem("  " + item + "  ") {
			t.Fatalf("含首尾空白的合法诊断项 %q 被拒", item)
		}
	}
	if ValidDiagItem("unknown_item") || ValidDiagItem("") {
		t.Fatal("未知或空诊断项不应通过校验")
	}
	if got := NormalizeDiagItem("  PORTS "); got != DiagItemPorts {
		t.Fatalf("NormalizeDiagItem 得到 %q", got)
	}

	now := time.Now().UTC().Truncate(time.Second)
	res := DiagRunResult{
		CollectedAt: now,
		NodeTimeUTC: now,
		Items: []DiagItemResult{
			{
				Name:          DiagItemLogs,
				OK:            true,
				Status:        DiagStatusOK,
				DurationMS:    12,
				ContentType:   "data",
				UntrustedData: true,
				Data:          map[string]any{"lines": []string{"line1"}},
			},
			{
				Name:   DiagItemSpeedtest,
				OK:     true,
				Status: DiagStatusNotAvailable,
				Data:   map[string]any{"message": "测速诊断项将在 M7 接入"},
			},
		},
	}
	raw, err := json.Marshal(res)
	if err != nil {
		t.Fatal(err)
	}
	var back DiagRunResult
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	if len(back.Items) != 2 || !back.Items[0].UntrustedData || back.Items[0].ContentType != "data" || back.Items[1].Status != DiagStatusNotAvailable {
		t.Fatalf("往返异常：%+v", back)
	}
}
