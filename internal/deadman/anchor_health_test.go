package deadman

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// anchorHealthEngine wires an engine whose anchor endpoint returns whatever
// the caller decides for a given "since", with probes forced healthy so the
// only thing under test is the anchor path.
func anchorHealthEngine(t *testing.T, respond func(since string) []PulledAnchor) (*Engine, Config) {
	t.Helper()
	pki := newTestPKI(t, false)
	tokenPath := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenPath, []byte("anchor-secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer anchor-secret" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"anchors": respond(r.URL.Query().Get("since"))})
	}))
	server.TLS = &tls.Config{Certificates: []tls.Certificate{pki.serverCertificate}, MinVersion: tls.VersionTLS12}
	server.StartTLS()
	t.Cleanup(server.Close)

	cfg := anchorTestConfig(t, t.TempDir(), server.URL, server.URL, pki.caFile, tokenPath)
	engine, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	engine.check = func(context.Context, Config, TargetConfig) (time.Duration, string) { return time.Millisecond, "" }
	return engine, cfg
}

func anchorAuditReasons(t *testing.T, cfg Config) []string {
	t.Helper()
	payload, err := os.ReadFile(cfg.AuditFile)
	if err != nil {
		t.Fatal(err)
	}
	reasons := make([]string, 0)
	for _, line := range splitJSONLines(payload) {
		var record struct {
			Action     string `json:"action"`
			Outcome    string `json:"outcome"`
			ReasonCode string `json:"reason_code"`
		}
		if json.Unmarshal(line, &record) != nil || record.Action != "deadman.anchor" {
			continue
		}
		if record.Outcome == "failure" {
			reasons = append(reasons, record.ReasonCode)
		}
	}
	return reasons
}

// TestAnchorSequenceRewindIsReported covers the failure mode that made the
// witness fall silent at exactly the wrong moment. If the watched instance is
// restored from an older backup its anchor sequence restarts, so every anchor
// it serves is at or below the high-water mark the puller already holds — and
// skipping those quietly meant the audit chain rolling back, the one event
// this whole mechanism exists to catch, produced no signal at all.
func TestAnchorSequenceRewindIsReported(t *testing.T) {
	stage := 0
	engine, cfg := anchorHealthEngine(t, func(string) []PulledAnchor {
		stage++
		if stage == 1 {
			return []PulledAnchor{
				{Sequence: 1, Records: 10, InstanceID: "ins_1", ObservedAt: time.Now().UTC()},
				{Sequence: 2, Records: 20, InstanceID: "ins_1", ObservedAt: time.Now().UTC()},
			}
		}
		// Restored from an older backup: sequences start over.
		return []PulledAnchor{
			{Sequence: 1, Records: 5, InstanceID: "ins_1", ObservedAt: time.Now().UTC()},
		}
	})

	if err := engine.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := engine.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	reasons := anchorAuditReasons(t, cfg)
	if !containsString(reasons, "anchor_sequence_rewound") {
		t.Fatalf("a rewound anchor sequence must be recorded: %v", reasons)
	}
	if engine.state.Targets["halro"].AnchorReason != "anchor_sequence_rewound" {
		t.Fatalf("anchor reason=%q, want it persisted so a restart does not read as recovery",
			engine.state.Targets["halro"].AnchorReason)
	}
}

// TestAnchorSequenceGapIsReported covers the other direction: the emitter
// keeps a bounded ring, so a puller that was away long enough comes back to
// anchors it can never retrieve. The gap is permanent, and reporting it is the
// difference between "the anchors I have agree" and "I have all the anchors".
func TestAnchorSequenceGapIsReported(t *testing.T) {
	engine, cfg := anchorHealthEngine(t, func(string) []PulledAnchor {
		return []PulledAnchor{
			{Sequence: 7, Records: 70, InstanceID: "ins_1", ObservedAt: time.Now().UTC()},
		}
	})
	if err := engine.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	reasons := anchorAuditReasons(t, cfg)
	if !containsString(reasons, "anchor_sequence_gap") {
		t.Fatalf("a skipped anchor sequence must be recorded: %v", reasons)
	}
}

// TestHealthyAnchorPullRecordsSuccess keeps the check from being trivially
// satisfiable: a clean pull must record a success, or "no failure recorded"
// would mean nothing.
func TestHealthyAnchorPullRecordsSuccess(t *testing.T) {
	engine, cfg := anchorHealthEngine(t, func(since string) []PulledAnchor {
		if since != "0" {
			return nil
		}
		return []PulledAnchor{{Sequence: 1, Records: 10, InstanceID: "ins_1", ObservedAt: time.Now().UTC()}}
	})
	if err := engine.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if reasons := anchorAuditReasons(t, cfg); len(reasons) != 0 {
		t.Fatalf("a clean pull recorded failures: %v", reasons)
	}
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func splitJSONLines(payload []byte) [][]byte {
	lines := make([][]byte, 0)
	start := 0
	for index, character := range payload {
		if character == '\n' {
			if index > start {
				lines = append(lines, payload[start:index])
			}
			start = index + 1
		}
	}
	if start < len(payload) {
		lines = append(lines, payload[start:])
	}
	return lines
}
