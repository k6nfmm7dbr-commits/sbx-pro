package protocol

import (
	"encoding/json"
	"testing"
)

func TestEnvelopeRoundTrip(t *testing.T) {
	e, err := New(MsgHeartbeat, "id-1", map[string]any{"machine_id": "m1"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if e.Version != ProtocolVersion {
		t.Errorf("version = %d, want %d", e.Version, ProtocolVersion)
	}
	if e.Type != MsgHeartbeat {
		t.Errorf("type = %s, want %s", e.Type, MsgHeartbeat)
	}
	data, err := e.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	got, err := UnmarshalEnvelope(data)
	if err != nil {
		t.Fatalf("UnmarshalEnvelope: %v", err)
	}
	if got.Type != MsgHeartbeat || got.ID != "id-1" {
		t.Errorf("roundtrip mismatch: type=%s id=%s", got.Type, got.ID)
	}
	var payload map[string]any
	if err := got.PayloadInto(&payload); err != nil {
		t.Fatalf("PayloadInto: %v", err)
	}
	if payload["machine_id"] != "m1" {
		t.Errorf("payload machine_id = %v, want m1", payload["machine_id"])
	}
}

func TestEnvelopeVersionMismatch(t *testing.T) {
	raw := `{"version":999,"type":"heartbeat","timestamp":0}`
	_, err := UnmarshalEnvelope([]byte(raw))
	if err == nil {
		t.Fatal("expected version error, got nil")
	}
	if _, ok := err.(*VersionError); !ok {
		t.Errorf("expected *VersionError, got %T", err)
	}
}

func TestEnvelopeMissingType(t *testing.T) {
	raw := `{"version":1,"timestamp":0}`
	_, err := UnmarshalEnvelope([]byte(raw))
	if err == nil {
		t.Fatal("expected malformed error, got nil")
	}
	if _, ok := err.(*MalformedError); !ok {
		t.Errorf("expected *MalformedError, got %T", err)
	}
}

func TestNewNilPayload(t *testing.T) {
	e, err := New(MsgRequestStatus, "id", nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if len(e.Payload) != 0 {
		t.Errorf("payload should be empty, got %s", string(e.Payload))
	}
}

func TestNewRawMessagePayload(t *testing.T) {
	raw := json.RawMessage(`{"a":1}`)
	e, err := New("custom", "id", raw)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if string(e.Payload) != `{"a":1}` {
		t.Errorf("payload = %s, want raw passthrough", string(e.Payload))
	}
}

func TestIsKnownType(t *testing.T) {
	known := []string{
		MsgHello, MsgHeartbeat, MsgTaskResult, MsgNodeStatus,
		MsgTrafficDelta, MsgTrafficSnapshot, MsgIPSync, MsgLogEvent, MsgSyncState,
		MsgCreateNode, MsgUpdateNode, MsgDeleteNode, MsgEnableNode, MsgDisableNode,
		MsgRestartSingbox, MsgSetQuota, MsgResetQuota, MsgSetIPLimit, MsgSyncConfig,
		MsgRequestStatus, MsgUpdateAgent, MsgHelloAck, MsgError,
	}
	for _, k := range known {
		if !IsKnownType(k) {
			t.Errorf("IsKnownType(%q) = false, want true", k)
		}
	}
	if IsKnownType("rm -rf") {
		t.Error("arbitrary command must not be a known type")
	}
}

func TestIsTaskType(t *testing.T) {
	taskTypes := []string{
		MsgCreateNode, MsgUpdateNode, MsgDeleteNode, MsgEnableNode, MsgDisableNode,
		MsgRestartSingbox, MsgSetQuota, MsgResetQuota, MsgSetIPLimit, MsgSyncConfig,
		MsgUpdateAgent,
	}
	for _, k := range taskTypes {
		if !IsTaskType(k) {
			t.Errorf("IsTaskType(%q) = false, want true", k)
		}
	}
	// 上报类消息不是任务
	for _, k := range []string{MsgHeartbeat, MsgHello, MsgTaskResult, MsgSyncState} {
		if IsTaskType(k) {
			t.Errorf("IsTaskType(%q) = true, want false", k)
		}
	}
}

func TestTaskResultJSON(t *testing.T) {
	tr := TaskResult{
		TaskID:    "t1",
		Status:    TaskSuccess,
		Message:   "ok",
		AppliedRevision: 3,
		CompletedAt: 12345,
	}
	data, err := json.Marshal(tr)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var back TaskResult
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if back.TaskID != "t1" || back.Status != TaskSuccess || back.AppliedRevision != 3 {
		t.Errorf("roundtrip mismatch: %+v", back)
	}
}

func TestTrafficDeltaFields(t *testing.T) {
	td := TrafficDelta{
		MachineID: "m1",
		Sequence:  42,
		RxBytes:   100,
		TxBytes:   200,
	}
	data, _ := json.Marshal(td)
	var back TrafficDelta
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if back.MachineID != "m1" || back.Sequence != 42 || back.RxBytes != 100 || back.TxBytes != 200 {
		t.Errorf("roundtrip mismatch: %+v", back)
	}
}
