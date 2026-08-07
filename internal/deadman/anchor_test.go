package deadman

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func anchorTestConfig(t *testing.T, directory, targetURL, anchorURL, caFile, tokenFile string) Config {
	t.Helper()
	cfg := validConfig(directory, targetURL, caFile, tokenFile)
	cfg.AnchorFile = filepath.Join(directory, "anchors.jsonl")
	cfg.Targets[0].AnchorURL = anchorURL
	return cfg
}

func TestValidateRejectsAnchorURLMisconfiguration(t *testing.T) {
	pki := newTestPKI(t, false)
	tokenPath := filepath.Join(t.TempDir(), "token")
	os.WriteFile(tokenPath, []byte("secret"), 0o600)
	directory := t.TempDir()
	base := validConfig(directory, "https://127.0.0.1:1/notify", pki.caFile, tokenPath)

	// anchor_url on a non-heimdall target.
	withWrongKind := base
	withWrongKind.AnchorFile = filepath.Join(directory, "anchors.jsonl")
	withWrongKind.Targets = append([]TargetConfig{}, base.Targets...)
	withWrongKind.Targets[1].AnchorURL = "https://127.0.0.1:1/anchors"
	if err := withWrongKind.Validate(); err == nil {
		t.Fatal("anchor_url on a prometheus target was accepted")
	}

	// anchor_url without anchor_file.
	withoutAnchorFile := base
	withoutAnchorFile.Targets = append([]TargetConfig{}, base.Targets...)
	withoutAnchorFile.Targets[0].AnchorURL = "https://127.0.0.1:1/anchors"
	if err := withoutAnchorFile.Validate(); err == nil {
		t.Fatal("anchor_url without anchor_file was accepted")
	}

	// A valid configuration.
	valid := base
	valid.AnchorFile = filepath.Join(directory, "anchors.jsonl")
	valid.Targets = append([]TargetConfig{}, base.Targets...)
	valid.Targets[0].AnchorURL = "https://127.0.0.1:1/anchors"
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid anchor configuration was rejected: %v", err)
	}
}

// TestPullAnchorsFetchesAndPersistsIncrementally is the ADR 0015 dead-man
// pull path end to end: a probe fetches new anchors since its last known
// sequence, appends them locally, and advances its high-water mark so the
// next pull only asks for what it does not already have.
func TestPullAnchorsFetchesAndPersistsIncrementally(t *testing.T) {
	pki := newTestPKI(t, false)
	tokenPath := filepath.Join(t.TempDir(), "token")
	os.WriteFile(tokenPath, []byte("anchor-secret"), 0o600)
	var requests atomic.Int32
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer anchor-secret" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		requests.Add(1)
		since := r.URL.Query().Get("since")
		var anchors []PulledAnchor
		if since == "0" {
			anchors = []PulledAnchor{
				{Sequence: 1, Records: 10, InstanceID: "ins_1", ObservedAt: time.Now().UTC()},
				{Sequence: 2, Records: 20, InstanceID: "ins_1", ObservedAt: time.Now().UTC()},
			}
		} else if since == "2" {
			anchors = []PulledAnchor{
				{Sequence: 3, Records: 30, InstanceID: "ins_1", ObservedAt: time.Now().UTC()},
			}
		}
		json.NewEncoder(w).Encode(map[string]any{"anchors": anchors})
	}))
	server.TLS = &tls.Config{Certificates: []tls.Certificate{pki.serverCertificate}, MinVersion: tls.VersionTLS12}
	server.StartTLS()
	defer server.Close()

	directory := t.TempDir()
	cfg := anchorTestConfig(t, directory, server.URL, server.URL, pki.caFile, tokenPath)
	engine, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	engine.check = func(context.Context, Config, TargetConfig) (time.Duration, string) { return time.Millisecond, "" }

	if err := engine.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := engine.state.Targets["heimdall"].LastAnchorSequence; got != 2 {
		t.Fatalf("after first pull, LastAnchorSequence=%d, want 2", got)
	}
	if err := engine.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := engine.state.Targets["heimdall"].LastAnchorSequence; got != 3 {
		t.Fatalf("after second pull, LastAnchorSequence=%d, want 3 (incremental since=2 not since=0)", got)
	}
	if requests.Load() != 2 {
		t.Fatalf("requests=%d, want 2", requests.Load())
	}

	payload, err := os.ReadFile(cfg.AnchorFile)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadAnchorsFileForTest(cfg.AnchorFile)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded) != 3 || loaded[0].Sequence != 1 || loaded[2].Sequence != 3 {
		t.Fatalf("persisted anchors=%#v raw=%q", loaded, payload)
	}

	// State survives a restart: a fresh engine loading the same state file
	// must not re-pull anchors it already has, and a stale anchor must never
	// be replayed as current.
	reloaded, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if got := reloaded.state.Targets["heimdall"].LastAnchorSequence; got != 3 {
		t.Fatalf("reloaded LastAnchorSequence=%d, want 3", got)
	}
}

// TestPullAnchorsUnreachableDoesNotBlockProbes is the fail-open half of ADR
// 0015: an unreachable anchor sink must not stall or fail the Tick that also
// carries the heartbeat and the other probes.
func TestPullAnchorsUnreachableDoesNotBlockProbes(t *testing.T) {
	pki := newTestPKI(t, false)
	tokenPath := filepath.Join(t.TempDir(), "token")
	os.WriteFile(tokenPath, []byte("anchor-secret"), 0o600)
	directory := t.TempDir()
	// A URL with no listener behind it — every pull fails with a connection
	// error, the way an unreachable Heimdall instance would behave.
	cfg := anchorTestConfig(t, directory, "https://127.0.0.1:1/notify", "https://127.0.0.1:1/anchors", pki.caFile, tokenPath)
	engine, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	var checks atomic.Int32
	engine.check = func(context.Context, Config, TargetConfig) (time.Duration, string) {
		checks.Add(1)
		return time.Millisecond, ""
	}
	if err := engine.Tick(context.Background()); err != nil {
		t.Fatalf("Tick failed because of an unreachable anchor sink: %v", err)
	}
	if checks.Load() != 3 {
		t.Fatalf("probes ran=%d, want 3 — anchor pull failure must not skip probing", checks.Load())
	}
	if got := engine.state.Targets["heimdall"].LastAnchorSequence; got != 0 {
		t.Fatalf("LastAnchorSequence=%d after a failed pull, want 0", got)
	}
	if _, err := os.Stat(cfg.AnchorFile); !os.IsNotExist(err) {
		t.Fatalf("anchor file was created despite every pull failing: err=%v", err)
	}
}

// The witness writes to its own disk forever. Without a cap it eventually
// fills the volume, and a probe that has filled its disk has stopped
// witnessing — the failure it is deployed to detect, happening to itself.
// Rotation has to keep the retired generation whole, because the value of the
// series is its continuity.
func TestAnchorWriterRotatesAtTheCapAndKeepsTheRetiredGeneration(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "anchors.jsonl")
	writer := &anchorWriter{path: path}

	// Exactly at the cap: the size is checked before writing, so this is the
	// append that rotates.
	if err := os.WriteFile(path, bytes.Repeat([]byte("x"), maxAnchorFileBytes), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writer.append(PulledAnchor{Sequence: 1, Records: 10, InstanceID: "ins_1", ObservedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(path); err != nil || info.Size() >= maxAnchorFileBytes {
		t.Fatalf("live file was not rotated: size=%v err=%v", info, err)
	}
	if _, err := os.Stat(path + rotatedAnchorSuffix); err != nil {
		t.Fatalf("retired generation is missing: %v", err)
	}

	// The pre-rotation content is intact, not truncated in place.
	retired, err := os.ReadFile(path + rotatedAnchorSuffix)
	if err != nil {
		t.Fatal(err)
	}
	if len(retired) != maxAnchorFileBytes {
		t.Fatalf("retired generation size=%d, want %d — rotation lost data", len(retired), maxAnchorFileBytes)
	}
	live, err := LoadAnchorsFileForTest(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(live) != 1 || live[0].Sequence != 1 {
		t.Fatalf("live file after rotation=%#v, want just the anchor that triggered it", live)
	}

	// A second rotation replaces the one retired generation rather than
	// accumulating .2, .3 — the bound is two files, not a growing set.
	if err := os.WriteFile(path, bytes.Repeat([]byte("y"), maxAnchorFileBytes), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writer.append(PulledAnchor{Sequence: 2, Records: 20, InstanceID: "ins_1", ObservedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		names := make([]string, 0, len(entries))
		for _, entry := range entries {
			names = append(names, entry.Name())
		}
		t.Fatalf("anchor files=%v, want exactly the live file and one retired generation", names)
	}
}

// LoadAnchorsFileForTest reads back what anchorWriter wrote, mirroring
// app.LoadAuditAnchorsFile's JSON-lines format without importing the app
// package (which would be a layering inversion — internal/app depends on
// internal/deadman's shapes only through matching JSON tags, never the
// reverse).
func LoadAnchorsFileForTest(path string) ([]PulledAnchor, error) {
	payload, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	var anchors []PulledAnchor
	for {
		var anchor PulledAnchor
		if err := decoder.Decode(&anchor); err != nil {
			if err == io.EOF {
				break
			}
			return nil, err
		}
		anchors = append(anchors, anchor)
	}
	return anchors, nil
}
