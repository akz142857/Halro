package audit

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type auditUnboundedCodec struct{ called *bool }

func (v auditUnboundedCodec) MarshalJSON() ([]byte, error) {
	*v.called = true
	return []byte(`"unexpected"`), nil
}

func TestAuditRejectsUnboundedInputBeforeCodecOrNativeWrite(t *testing.T) {
	log, err := Open(filepath.Join(t.TempDir(), "audit.log"), make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	for _, fault := range []string{"wide-field", "wide-metadata", "cycle", "callback"} {
		t.Run(fault, func(t *testing.T) {
			called := false
			event := Event{EventID: "bounded", ActorType: "system", Action: "test", Outcome: "success", OccurredAt: time.Now().UTC()}
			switch fault {
			case "wide-field":
				event.EventID = strings.Repeat("x", MaxBatchPayloadBytes+1)
			case "wide-metadata":
				event.Metadata = map[string]any{"wide": strings.Repeat("x", 8<<10)}
			case "cycle":
				event.Metadata = map[string]any{}
				event.Metadata["cycle"] = event.Metadata
			case "callback":
				event.Metadata = map[string]any{"value": auditUnboundedCodec{&called}}
			}
			before := log.Summary()
			if _, err := log.Append(context.Background(), event); !errors.Is(err, ErrBatchLimit) {
				t.Fatal(err)
			}
			if called || log.Summary() != before {
				t.Fatal("rejected input ran its codec or changed native facts")
			}
		})
	}
}

func TestAuditInputBoundPreservesPlainMetadataAndLargeNumbers(t *testing.T) {
	now := time.Now().UTC()
	event := Event{EventID: "bounded", ActorType: "system", Action: "test", Outcome: "success", OccurredAt: now, Metadata: map[string]any{"at": &now, "raw": json.RawMessage(`{"n":18446744073709551615}`), "number": json.Number("18446744073709551615"), "rows": []any{true, nil, map[string]string{"x": "y"}}}}
	if err := event.Validate(); err != nil {
		t.Fatal(err)
	}
}
