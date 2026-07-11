package spawn

import (
	"strings"
	"testing"
	"time"
)

func validEnvelope() Envelope {
	return Envelope{
		SchemaVersion:   SchemaVersion,
		TaskID:          "t_deadbeef",
		TraceID:         "abcdef0123456789abcdef0123456789",
		SessionID:       nil,
		Kind:            "chat",
		Prompt:          "merhaba",
		Model:           "claude-sonnet-5",
		MemoryInjection: true,
		CreatedAt:       time.Now().UTC().Format(time.RFC3339),
	}
}

func TestValidateAcceptsWellFormedEnvelope(t *testing.T) {
	if err := validEnvelope().Validate(); err != nil {
		t.Fatalf("Validate() error = %v, want nil", err)
	}
}

func TestValidateRejectsBadSchemaVersion(t *testing.T) {
	e := validEnvelope()
	e.SchemaVersion = 2
	if err := e.Validate(); err == nil {
		t.Fatal("Validate() = nil, want error for wrong schema_version")
	}
}

func TestValidateRejectsEmptyTaskID(t *testing.T) {
	e := validEnvelope()
	e.TaskID = "  "
	if err := e.Validate(); err == nil {
		t.Fatal("Validate() = nil, want error for empty task_id")
	}
}

func TestValidateRejectsEmptyTraceID(t *testing.T) {
	e := validEnvelope()
	e.TraceID = ""
	if err := e.Validate(); err == nil {
		t.Fatal("Validate() = nil, want error for empty trace_id")
	}
}

func TestValidateRejectsWrongKind(t *testing.T) {
	e := validEnvelope()
	e.Kind = "batch"
	if err := e.Validate(); err == nil {
		t.Fatal("Validate() = nil, want error for kind != chat")
	}
}

func TestValidateRejectsEmptyPrompt(t *testing.T) {
	e := validEnvelope()
	e.Prompt = "   "
	if err := e.Validate(); err == nil {
		t.Fatal("Validate() = nil, want error for blank prompt")
	}
}

func TestValidateRejectsUnknownModel(t *testing.T) {
	e := validEnvelope()
	e.Model = "gpt-5"
	if err := e.Validate(); err == nil {
		t.Fatal("Validate() = nil, want error for model outside AllowedModels")
	}
}

func TestValidateAcceptsEveryAllowedModel(t *testing.T) {
	for model := range AllowedModels {
		e := validEnvelope()
		e.Model = model
		if err := e.Validate(); err != nil {
			t.Errorf("Validate() with model=%q error = %v, want nil", model, err)
		}
	}
}

func TestValidateRejectsNonRFC3339CreatedAt(t *testing.T) {
	e := validEnvelope()
	e.CreatedAt = "2026-07-10 12:00:00"
	if err := e.Validate(); err == nil {
		t.Fatal("Validate() = nil, want error for non-RFC3339 created_at")
	}
}

func TestMarshalRendersNullSessionID(t *testing.T) {
	e := validEnvelope()
	b, err := e.Marshal()
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	if !strings.Contains(string(b), `"session_id":null`) {
		t.Errorf("Marshal() = %s, want session_id:null present", b)
	}
}

func TestMarshalPreservesTurkishPromptByteExact(t *testing.T) {
	e := validEnvelope()
	e.Prompt = "Kadıköy'deki randevuyu hatırlat"
	b, err := e.Marshal()
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	if !strings.Contains(string(b), "Kadıköy'deki randevuyu hatırlat") {
		t.Errorf("Marshal() did not preserve the Turkish prompt byte-exact: %s", b)
	}
}

// --- W3-08: envelope lane pinning ---

func TestValidateAcceptsEmptyLane(t *testing.T) {
	e := validEnvelope() // Lane left at its zero value ("")
	if err := e.Validate(); err != nil {
		t.Fatalf("Validate() error = %v, want nil (empty lane treated as normal)", err)
	}
}

func TestValidateAcceptsSecretAndNormalLane(t *testing.T) {
	for _, lane := range []string{LaneSecret, LaneNormal} {
		e := validEnvelope()
		e.Lane = lane
		if err := e.Validate(); err != nil {
			t.Errorf("Validate() with lane=%q error = %v, want nil", lane, err)
		}
	}
}

func TestValidateRejectsUnknownLane(t *testing.T) {
	e := validEnvelope()
	e.Lane = "cloud"
	if err := e.Validate(); err == nil {
		t.Fatal("Validate() = nil, want error for an unrecognized lane value")
	}
}

func TestMarshalRoundTripsLaneAndCategory(t *testing.T) {
	e := validEnvelope()
	e.Lane = LaneSecret
	e.Category = "saglik"
	b, err := e.Marshal()
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	if !strings.Contains(string(b), `"lane":"secret"`) {
		t.Errorf("Marshal() = %s, want lane:secret present", b)
	}
	if !strings.Contains(string(b), `"category":"saglik"`) {
		t.Errorf("Marshal() = %s, want category:saglik present", b)
	}
}

// --- W4-02: envelope session resume ---

func TestValidateRejectsResumeWithoutSessionID(t *testing.T) {
	e := validEnvelope()
	e.Resume = true // SessionID left nil
	if err := e.Validate(); err == nil {
		t.Fatal("Validate() = nil, want error for resume=true with no session_id")
	}
}

func TestValidateRejectsResumeWithBlankSessionID(t *testing.T) {
	e := validEnvelope()
	blank := "   "
	e.SessionID = &blank
	e.Resume = true
	if err := e.Validate(); err == nil {
		t.Fatal("Validate() = nil, want error for resume=true with a blank session_id")
	}
}

func TestValidateAcceptsResumeWithSessionID(t *testing.T) {
	e := validEnvelope()
	sid := "sess-123"
	e.SessionID = &sid
	e.Resume = true
	if err := e.Validate(); err != nil {
		t.Fatalf("Validate() error = %v, want nil", err)
	}
}

func TestMarshalRoundTripsResumeAndSessionID(t *testing.T) {
	e := validEnvelope()
	sid := "sess-abc"
	e.SessionID = &sid
	e.Resume = true
	b, err := e.Marshal()
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	if !strings.Contains(string(b), `"resume":true`) {
		t.Errorf("Marshal() = %s, want resume:true present", b)
	}
	if !strings.Contains(string(b), `"session_id":"sess-abc"`) {
		t.Errorf("Marshal() = %s, want session_id:\"sess-abc\" present", b)
	}
}

func TestNewTaskIDShapeAndUniqueness(t *testing.T) {
	a := NewTaskID()
	b := NewTaskID()
	if !strings.HasPrefix(a, "t_") {
		t.Errorf("NewTaskID() = %q, want t_ prefix", a)
	}
	if len(a) != len("t_")+32 {
		t.Errorf("NewTaskID() = %q, want t_ + 32 hex chars", a)
	}
	if a == b {
		t.Error("NewTaskID() returned the same value twice")
	}
}

func TestNewAPIKeyShapeAndUniqueness(t *testing.T) {
	a := NewAPIKey()
	b := NewAPIKey()
	if !strings.HasPrefix(a, "kahya-task-") {
		t.Errorf("NewAPIKey() = %q, want kahya-task- prefix", a)
	}
	if a == b {
		t.Error("NewAPIKey() returned the same value twice")
	}
}
