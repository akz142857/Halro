package modelcatalog

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ed25519"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/domain"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) { return f(request) }

func catalogResponse(status int, body []byte, encoding string) *http.Response {
	response := &http.Response{StatusCode: status, Body: io.NopCloser(bytes.NewReader(body)), Header: make(http.Header), ContentLength: int64(len(body))}
	if encoding != "" {
		response.Header.Set("Content-Encoding", encoding)
	}
	return response
}

func managerForTest(t *testing.T, now time.Time, privateKey ed25519.PrivateKey, root TrustRoot, transport http.RoundTripper, enabled bool) (*Manager, func(uint64) []byte) {
	t.Helper()
	payload := func(sequence uint64) []byte {
		return signedSnapshotJSON(t, testSnapshot(now, sequence, dynamicOpenAIEntry("gpt-dynamic")), root.KeyID, privateKey)
	}
	manager, err := NewManager(ManagerOptions{
		Enabled: enabled, RefreshInterval: time.Hour, DataDir: t.TempDir(), TrustRoots: []TrustRoot{root},
		MaxDownloadBytes: 64 << 10, MaxDecodedBytes: 128 << 10, MaxCompressionRatio: 20, MaxEntries: 100,
		Now: func() time.Time { return now }, HTTPClient: &http.Client{Transport: transport}, Endpoint: "https://catalog.test/v1.json",
	})
	if err != nil {
		t.Fatal(err)
	}
	return manager, payload
}

func TestDisabledManagerPerformsZeroNetworkRequests(t *testing.T) {
	now := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)
	privateKey, root := testSigningKey(t, "root", now)
	var calls atomic.Int64
	manager, _ := managerForTest(t, now, privateKey, root, roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return nil, errors.New("network trap")
	}), false)
	manager.Run(context.Background())
	if calls.Load() != 0 || manager.Status().State != CatalogStateDisabled {
		t.Fatalf("disabled manager calls=%d status=%#v", calls.Load(), manager.Status())
	}
}

func TestManagerPersistsAndReloadsLastKnownGood(t *testing.T) {
	now := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)
	privateKey, root := testSigningKey(t, "root", now)
	directory := t.TempDir()
	payload := signedSnapshotJSON(t, testSnapshot(now, 7, dynamicOpenAIEntry("gpt-dynamic")), root.KeyID, privateKey)
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) { return catalogResponse(http.StatusOK, payload, ""), nil })}
	options := ManagerOptions{Enabled: true, DataDir: directory, TrustRoots: []TrustRoot{root}, Now: func() time.Time { return now }, HTTPClient: client, Endpoint: "https://catalog.test", MaxEntries: 100}
	manager, err := NewManager(options)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if manager.Status().Source != SourceSignedCatalog || manager.Status().Sequence != 7 {
		t.Fatalf("status=%#v", manager.Status())
	}
	if info, err := os.Stat(filepath.Join(directory, LastKnownGoodFile)); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("last-known-good permissions info=%v err=%v", info, err)
	}
	options.HTTPClient = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) { return nil, errors.New("offline") })}
	restarted, err := NewManager(options)
	if err != nil {
		t.Fatal(err)
	}
	entry, found := restarted.Current().Lookup(Key{ProviderType: dynamicOpenAIEntry("x").ProviderType, Profile: dynamicOpenAIEntry("x").ProfileID, TargetKind: domain.TargetModelID, Model: "gpt-dynamic"})
	if !found || entry.Source != SourceSignedCatalog || restarted.Status().Sequence != 7 {
		t.Fatalf("LKG was not restored: entry=%#v status=%#v", entry, restarted.Status())
	}
}

func TestDisabledManagerIgnoresPreexistingSignedLastKnownGood(t *testing.T) {
	now := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)
	privateKey, root := testSigningKey(t, "root", now)
	directory := t.TempDir()
	payload := signedSnapshotJSON(t, testSnapshot(now, 3, dynamicOpenAIEntry("gpt-dynamic")), root.KeyID, privateKey)
	options := ManagerOptions{Enabled: true, DataDir: directory, TrustRoots: []TrustRoot{root}, Now: func() time.Time { return now }, HTTPClient: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return catalogResponse(http.StatusOK, payload, ""), nil
	})}, Endpoint: "https://catalog.test", MaxEntries: 100}
	enabled, err := NewManager(options)
	if err != nil {
		t.Fatal(err)
	}
	if err := enabled.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	options.Enabled = false
	disabled, err := NewManager(options)
	if err != nil {
		t.Fatal(err)
	}
	status := disabled.Status()
	if status.State != CatalogStateDisabled || status.Source != SourceBuiltin || disabled.Current().Revision() != Builtin().Revision() {
		t.Fatalf("disabled manager activated LKG: status=%#v", status)
	}
}

func TestManagerKeepsExpiredVerifiedLastKnownGoodVisibleButNotResolutionEffective(t *testing.T) {
	now := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)
	privateKey, root := testSigningKey(t, "root", now)
	root.NotAfter = now.Add(24 * time.Hour)
	directory := t.TempDir()
	payload := signedSnapshotJSON(t, testSnapshot(now, 1, dynamicOpenAIEntry("gpt-dynamic")), root.KeyID, privateKey)
	options := ManagerOptions{
		Enabled: true, DataDir: directory, TrustRoots: []TrustRoot{root}, MaxEntries: 100,
		Now: func() time.Time { return now }, HTTPClient: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return catalogResponse(http.StatusOK, payload, ""), nil
		})}, Endpoint: "https://catalog.test",
	}
	manager, err := NewManager(options)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	options.Now = func() time.Time { return now.Add(2 * time.Hour) }
	restarted, err := NewManager(options)
	if err != nil {
		t.Fatal(err)
	}
	status := restarted.Status()
	if status.State != CatalogStateDegraded || status.Source != SourceSignedCatalog || !status.UsingExpiredLKG {
		t.Fatalf("expired LKG status=%#v", status)
	}
	if _, found := restarted.Current().Lookup(Key{ProviderType: domain.ProviderOpenAI, Profile: domain.ProfileOpenAIChatEmbeddings, TargetKind: domain.TargetModelID, Model: "gpt-dynamic"}); found {
		t.Fatal("expired LKG remained resolution-effective")
	}
}

func TestRunningManagerStopsResolvingFromCatalogAtExpiry(t *testing.T) {
	now := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)
	current := now
	privateKey, root := testSigningKey(t, "root", now)
	root.NotAfter = now.Add(24 * time.Hour)
	payload := signedSnapshotJSON(t, testSnapshot(now, 1, dynamicOpenAIEntry("gpt-dynamic")), root.KeyID, privateKey)
	manager, err := NewManager(ManagerOptions{
		Enabled: true, DataDir: t.TempDir(), TrustRoots: []TrustRoot{root}, MaxEntries: 100,
		Now: func() time.Time { return current }, HTTPClient: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return catalogResponse(http.StatusOK, payload, ""), nil
		})}, Endpoint: "https://catalog.test",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	current = now.Add(2 * time.Hour)
	if _, found := manager.Current().Lookup(Key{ProviderType: domain.ProviderOpenAI, Profile: domain.ProfileOpenAIChatEmbeddings, TargetKind: domain.TargetModelID, Model: "gpt-dynamic"}); found {
		t.Fatal("catalog remained resolution-effective after runtime expiry")
	}
	if status := manager.Status(); status.State != CatalogStateDegraded || status.ErrorClass != "expired" || !status.UsingExpiredLKG {
		t.Fatalf("expiry status=%#v", status)
	}
}

func TestSequenceCheckpointRefusesReplayAfterLastKnownGoodLoss(t *testing.T) {
	now := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)
	privateKey, root := testSigningKey(t, "root", now)
	directory := t.TempDir()
	payload := signedSnapshotJSON(t, testSnapshot(now, 7, dynamicOpenAIEntry("gpt-v7")), root.KeyID, privateKey)
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) { return catalogResponse(http.StatusOK, payload, ""), nil })}
	options := ManagerOptions{Enabled: true, DataDir: directory, TrustRoots: []TrustRoot{root}, Now: func() time.Time { return now }, HTTPClient: client, Endpoint: "https://catalog.test", MaxEntries: 100}
	manager, err := NewManager(options)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(directory, LastKnownGoodFile)); err != nil {
		t.Fatal(err)
	}
	payload = signedSnapshotJSON(t, testSnapshot(now, 6, dynamicOpenAIEntry("gpt-v6")), root.KeyID, privateKey)
	restarted, err := NewManager(options)
	if err != nil {
		t.Fatal(err)
	}
	if err := restarted.Refresh(context.Background()); err == nil || restarted.Status().ErrorClass != "rollback" {
		t.Fatalf("checkpoint replay err=%v status=%#v", err, restarted.Status())
	}
}

func TestActivationPreparationFailureKeepsPreviousCatalogAndLKG(t *testing.T) {
	now := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)
	privateKey, root := testSigningKey(t, "root", now)
	manager, payload := managerForTest(t, now, privateKey, root, roundTripFunc(func(*http.Request) (*http.Response, error) {
		return catalogResponse(http.StatusOK, signedSnapshotJSON(t, testSnapshot(now, 1, dynamicOpenAIEntry("gpt-dynamic")), root.KeyID, privateKey), ""), nil
	}), true)
	_ = payload
	manager.SetActivationPreparer(func(*Catalog) (func(bool), error) { return nil, errors.New("registry build failed") })
	if err := manager.Refresh(context.Background()); err == nil || manager.Status().ErrorClass != "activation" {
		t.Fatalf("activation err=%v status=%#v", err, manager.Status())
	}
	if _, err := os.Stat(manager.lkgPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed candidate was persisted: %v", err)
	}
}

func TestActivationCommitRefusesChangedSequenceCheckpoint(t *testing.T) {
	now := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)
	privateKey, root := testSigningKey(t, "root", now)
	payload := signedSnapshotJSON(t, testSnapshot(now, 1, dynamicOpenAIEntry("gpt-dynamic")), root.KeyID, privateKey)
	manager, err := NewManager(ManagerOptions{
		Enabled: true, DataDir: t.TempDir(), TrustRoots: []TrustRoot{root}, MaxEntries: 100,
		Now: func() time.Time { return now }, Endpoint: "https://catalog.test",
		HTTPClient: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return catalogResponse(http.StatusOK, payload, ""), nil
		})},
	})
	if err != nil {
		t.Fatal(err)
	}
	committed := false
	manager.SetActivationPreparer(func(*Catalog) (func(bool), error) {
		manager.statusMu.Lock()
		manager.checkpoint = catalogCheckpoint{Sequence: 9, Revision: "sha256:newer"}
		manager.statusMu.Unlock()
		return func(commit bool) { committed = commit }, nil
	})
	if err := manager.Refresh(context.Background()); err == nil || manager.Status().ErrorClass != "activation" {
		t.Fatalf("stale activation err=%v status=%#v", err, manager.Status())
	}
	if committed {
		t.Fatal("stale candidate committed dependent state")
	}
	if _, err := os.Stat(manager.lkgPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stale candidate was persisted: %v", err)
	}
}

func TestManagerRejectsRollbackAndSequenceReuse(t *testing.T) {
	now := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)
	privateKey, root := testSigningKey(t, "root", now)
	var mu sync.Mutex
	current := signedSnapshotJSON(t, testSnapshot(now, 2, dynamicOpenAIEntry("gpt-v2")), root.KeyID, privateKey)
	transport := roundTripFunc(func(*http.Request) (*http.Response, error) {
		mu.Lock()
		defer mu.Unlock()
		return catalogResponse(http.StatusOK, current, ""), nil
	})
	manager, _ := managerForTest(t, now, privateKey, root, transport, true)
	if err := manager.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	current = signedSnapshotJSON(t, testSnapshot(now, 1, dynamicOpenAIEntry("gpt-v1")), root.KeyID, privateKey)
	mu.Unlock()
	if err := manager.Refresh(context.Background()); err == nil || manager.Status().ErrorClass != "rollback" {
		t.Fatalf("rollback err=%v status=%#v", err, manager.Status())
	}
	mu.Lock()
	current = signedSnapshotJSON(t, testSnapshot(now, 2, dynamicOpenAIEntry("gpt-different")), root.KeyID, privateKey)
	mu.Unlock()
	if err := manager.Refresh(context.Background()); err == nil || manager.Status().ErrorClass != "rollback" {
		t.Fatalf("sequence reuse err=%v status=%#v", err, manager.Status())
	}
}

func TestManagerBoundsCompressedAndDecodedPayloads(t *testing.T) {
	now := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)
	privateKey, root := testSigningKey(t, "root", now)
	valid := signedSnapshotJSON(t, testSnapshot(now, 1, dynamicOpenAIEntry("gpt-dynamic")), root.KeyID, privateKey)
	var compressed bytes.Buffer
	zipper := gzip.NewWriter(&compressed)
	_, _ = zipper.Write(valid)
	_ = zipper.Close()
	tests := []struct {
		name     string
		body     []byte
		encoding string
		download int64
		decoded  int64
		ratio    int64
	}{
		{"compressed bytes", bytes.Repeat([]byte("x"), 1025), "", 1024, 2048, 20},
		{"decoded bytes", valid, "", int64(len(valid) + 1), int64(len(valid) - 1), 20},
		{"gzip ratio", compressed.Bytes(), "gzip", int64(compressed.Len() + 1), int64(len(valid) + 1), 1},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			manager, err := NewManager(ManagerOptions{Enabled: true, DataDir: t.TempDir(), TrustRoots: []TrustRoot{root}, Now: func() time.Time { return now }, HTTPClient: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				return catalogResponse(http.StatusOK, test.body, test.encoding), nil
			})}, Endpoint: "https://catalog.test", MaxDownloadBytes: test.download, MaxDecodedBytes: test.decoded, MaxCompressionRatio: test.ratio, MaxEntries: 100})
			if err != nil {
				t.Fatal(err)
			}
			if err := manager.Refresh(context.Background()); err == nil {
				t.Fatal("oversized catalog was accepted")
			}
		})
	}
}

func TestManagerDegradesAndRecoversWithoutDroppingBundledCatalog(t *testing.T) {
	now := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)
	privateKey, root := testSigningKey(t, "root", now)
	var fail atomic.Bool
	fail.Store(true)
	recoveryPayload := signedSnapshotJSON(t, testSnapshot(now, 1, dynamicOpenAIEntry("gpt-dynamic")), root.KeyID, privateKey)
	manager, payload := managerForTest(t, now, privateKey, root, roundTripFunc(func(*http.Request) (*http.Response, error) {
		if fail.Load() {
			return nil, errors.New("offline")
		}
		return catalogResponse(http.StatusOK, recoveryPayload, ""), nil
	}), true)
	_ = payload
	var events []RefreshEvent
	manager.SetObserver(func(event RefreshEvent) { events = append(events, event) })
	if err := manager.Refresh(context.Background()); err == nil || manager.Status().State != CatalogStateUnavailable || manager.Current().Len() != Builtin().Len() {
		t.Fatalf("failure did not preserve bundled catalog: err=%v status=%#v", err, manager.Status())
	}
	fail.Store(false)
	if err := manager.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if manager.Status().State != CatalogStateCurrent || len(events) != 2 || !events[1].Recovered {
		t.Fatalf("recovery status=%#v events=%#v", manager.Status(), events)
	}
}

func TestManagerRejectsRedirectResponse(t *testing.T) {
	now := time.Now().UTC()
	privateKey, root := testSigningKey(t, "root", now)
	manager, _ := managerForTest(t, now, privateKey, root, roundTripFunc(func(*http.Request) (*http.Response, error) {
		response := catalogResponse(http.StatusFound, []byte("redirect"), "")
		response.Header.Set("Location", "http://127.0.0.1/latest")
		return response, nil
	}), true)
	if err := manager.Refresh(context.Background()); err == nil {
		t.Fatalf("redirect was not rejected: %v", err)
	}
}
