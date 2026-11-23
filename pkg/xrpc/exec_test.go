package xrpc

import (
	"encoding/json"
	"testing"
	"time"
)

func TestExecContractAndBackwardCompat(t *testing.T) {
	if ControlProtocol != 1 {
		t.Fatalf("ControlProtocol 必须保持 1 向后兼容，得到 %d", ControlProtocol)
	}
	if TypeExecEvent != "exec_event" || MethodDiagRun != "diag.run" || MethodExecStart != "exec.start" || MethodExecStatus != "exec.status" || MethodExecCancel != "exec.cancel" {
		t.Fatal("RPC 方法或消息类型常量不符合契约")
	}

	// 旧版节点不带 exec_enabled 字段时应默认解为 false（fail-closed）。
	var oldAuth Auth
	if err := json.Unmarshal([]byte(`{"token":"t","agent_version":"0.1.0","control_protocol":1}`), &oldAuth); err != nil {
		t.Fatal(err)
	}
	if oldAuth.ExecEnabled {
		t.Fatal("旧版节点缺少 exec_enabled 时必须默认 false")
	}

	// 新版节点携带 exec_enabled=true 时正常往返。
	ev := ExecEvent{
		JobID:      "job-0123456789abcdef",
		Seq:        3,
		Chunk:      "hello\n",
		Offset:     6,
		Done:       true,
		State:      ExecStateDone,
		ExitCode:   0,
		DurationMS: 42,
		At:         time.Now().UTC().Truncate(time.Millisecond),
	}
	raw, err := json.Marshal(ev)
	if err != nil {
		t.Fatal(err)
	}
	var got ExecEvent
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got.JobID != ev.JobID || got.Seq != 3 || got.Chunk != "hello\n" || !got.Done || got.State != ExecStateDone {
		t.Fatalf("ExecEvent 往返异常：%+v", got)
	}
}
