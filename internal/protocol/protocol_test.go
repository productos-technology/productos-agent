package protocol

import (
	"encoding/json"
	"testing"
)

func TestRunStartCarriesTheAttachments(t *testing.T) {
	raw := `{"type":"run.start","protocol_version":1,"run_id":"r1","tool":"claude","branch":"b",
	  "attachments":[{"id":"a1","filename":"Pricing.csv","content_type":"text/csv","byte_size":19,
	    "sha256":"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef","path":"/api/v1/agent/runs/r1/attachments/a1"}]}`
	var m ServerMessage
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatal(err)
	}
	start := m.AsRunStart()
	if len(start.Attachments) != 1 {
		t.Fatalf("attachments = %+v", start.Attachments)
	}
	if a := start.Attachments[0]; a.ID != "a1" || a.Filename != "Pricing.csv" || a.ContentType != "text/csv" || a.ByteSize != 19 || a.Path != "/api/v1/agent/runs/r1/attachments/a1" || len(a.SHA256) != 64 {
		t.Errorf("attachment = %+v", a)
	}
	// The handoff writes the message back as JSON: the attachments travel with it.
	again, _ := json.Marshal(m)
	var back ServerMessage
	_ = json.Unmarshal(again, &back)
	if len(back.AsRunStart().Attachments) != 1 {
		t.Error("the attachments are lost in a handoff")
	}
	if !(ServerMessage{Features: []string{"outcome", "attachments"}}).HasFeature(FeatureAttachments) {
		t.Error("hello's attachments feature is not read")
	}
}
