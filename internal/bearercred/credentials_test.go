package bearercred

import (
	"bytes"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestRotateOverlapRevokeAndRestore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "metrics-credentials.json")
	now := time.Date(2026, 8, 3, 0, 0, 0, 0, time.UTC)
	first, err := Rotate(path, time.Minute, now)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(first.Token)
	if ok, err := Authorize(path, string(first.Token), now); err != nil || !ok {
		t.Fatalf("first credential authorization: ok=%t err=%v", ok, err)
	}
	second, err := Rotate(path, time.Minute, now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	defer clear(second.Token)
	backup, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, token := range [][]byte{first.Token, second.Token} {
		if ok, err := Authorize(path, string(token), now.Add(2*time.Second)); err != nil || !ok {
			t.Fatalf("overlap authorization: ok=%t err=%v", ok, err)
		}
	}
	if ok, err := Authorize(path, string(first.Token), now.Add(2*time.Minute)); err != nil || ok {
		t.Fatalf("expired credential authorization: ok=%t err=%v", ok, err)
	}
	if err := Revoke(path, second.Version, now.Add(3*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if ok, err := Authorize(path, string(second.Token), now.Add(3*time.Minute)); err != nil || ok {
		t.Fatalf("revoked credential authorization: ok=%t err=%v", ok, err)
	}
	if err := os.WriteFile(path, backup, 0o600); err != nil {
		t.Fatal(err)
	}
	if ok, err := Authorize(path, string(second.Token), now.Add(3*time.Minute)); err != nil || ok {
		t.Fatalf("restored credential bypassed revocation ledger: ok=%t err=%v", ok, err)
	}
	third, err := Rotate(path, 0, now.Add(4*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	defer clear(third.Token)
	if third.Version <= second.Version {
		t.Fatalf("restored state reused revoked version: got=%d revoked=%d", third.Version, second.Version)
	}
	if ok, err := Authorize(path, string(third.Token), now.Add(4*time.Minute)); err != nil || !ok {
		t.Fatalf("post-restore rotation authorization: ok=%t err=%v", ok, err)
	}
}

func TestRejectsBroadPermissionsAndCorruptState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "metrics-credentials.json")
	rotation, err := Rotate(path, 0, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	clear(rotation.Token)
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("broad credential permissions were accepted")
	}
}

func TestCredentialAuditDetectsTamperTruncationReorderingAndDeletion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "metrics-credentials.json")
	now := time.Date(2026, 8, 3, 0, 0, 0, 0, time.UTC)
	first, err := Rotate(path, time.Minute, now)
	if err != nil {
		t.Fatal(err)
	}
	clear(first.Token)
	second, err := Rotate(path, time.Minute, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	clear(second.Token)
	if err := Revoke(path, second.Version, now.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := VerifyAudit(path); err != nil {
		t.Fatalf("valid audit rejected: %v", err)
	}
	original, err := os.ReadFile(auditPath(path))
	if err != nil {
		t.Fatal(err)
	}

	tampered := bytes.Replace(original, []byte(`"action":"rotate"`), []byte(`"action":"revoke"`), 1)
	if err := os.WriteFile(auditPath(path), tampered, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := VerifyAudit(path); err == nil {
		t.Fatal("tampered audit was accepted")
	}

	lines := bytes.Split(bytes.TrimSpace(original), []byte{'\n'})
	if err := os.WriteFile(auditPath(path), append(bytes.Join(lines[:len(lines)-1], []byte{'\n'}), '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := VerifyAudit(path); err == nil {
		t.Fatal("truncated audit was accepted")
	}

	reordered := append([][]byte(nil), lines...)
	reordered[0], reordered[1] = reordered[1], reordered[0]
	if err := os.WriteFile(auditPath(path), append(bytes.Join(reordered, []byte{'\n'}), '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := VerifyAudit(path); err == nil {
		t.Fatal("reordered audit was accepted")
	}

	if err := os.Remove(auditPath(path)); err != nil {
		t.Fatal(err)
	}
	if err := VerifyAudit(path); err == nil {
		t.Fatal("deleted audit was accepted")
	}
}

func TestRestoreCannotReuseVersionAnchoredInAudit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "metrics-credentials.json")
	now := time.Date(2026, 8, 3, 0, 0, 0, 0, time.UTC)
	first, err := Rotate(path, 0, now)
	if err != nil {
		t.Fatal(err)
	}
	clear(first.Token)
	backup, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	second, err := Rotate(path, 0, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	clear(second.Token)
	if err := os.WriteFile(path, backup, 0o600); err != nil {
		t.Fatal(err)
	}
	third, err := Rotate(path, 0, now.Add(2*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	clear(third.Token)
	if third.Version != 3 {
		t.Fatalf("restored state reused audit-anchored version: got=%d want=3", third.Version)
	}
}

func TestConcurrentRotationsAreSerialized(t *testing.T) {
	path := filepath.Join(t.TempDir(), "metrics-credentials.json")
	now := time.Date(2026, 8, 3, 0, 0, 0, 0, time.UTC)
	const count = 8
	versions := make(chan uint64, count)
	errorsChannel := make(chan error, count)
	var wait sync.WaitGroup
	for index := 0; index < count; index++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			rotation, err := Rotate(path, time.Minute, now)
			if err != nil {
				errorsChannel <- err
				return
			}
			clear(rotation.Token)
			versions <- rotation.Version
		}()
	}
	wait.Wait()
	close(errorsChannel)
	for err := range errorsChannel {
		t.Fatal(err)
	}
	close(versions)
	seen := make(map[uint64]struct{}, count)
	for version := range versions {
		seen[version] = struct{}{}
	}
	if len(seen) != count {
		t.Fatalf("concurrent rotations produced %d unique versions, want %d", len(seen), count)
	}
	if err := VerifyAudit(path); err != nil {
		t.Fatalf("concurrent rotation audit: %v", err)
	}
}
