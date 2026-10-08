package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// The second operation replaces the key-only history before its actual master
// key/unlock. A fresh process must authenticate the prior immutable capsule and
// the unfinished current capsule under different keys and still finish once.
func TestHAGroupRotationSecondActualSIGKILLHistoryBeforeUnlock(t *testing.T) {
	for _, mode := range []string{"file", "key_slots"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
			defer cancel()
			var f haMetadataMaintenanceFixture
			var owners []*haSelectedRuntimeFoundation
			var h *kmsAppHarness
			if mode == "file" {
				f, owners = prepareHAGroupRotationColdMembers(t, ctx)
			} else {
				f, owners, h = prepareHAGroupKMSColdMembers(t, ctx)
			}
			for _, o := range owners {
				if err := o.Close(); err != nil {
					t.Fatal(err)
				}
				if err := o.owners.Components.Guard.Close(); err != nil {
					t.Fatal(err)
				}
			}
			dir := t.TempDir()
			target := filepath.Join(dir, "next.key")
			first := bytes.Repeat([]byte{131}, 32)
			var err error
			if mode == "file" {
				if err := os.WriteFile(target, first, 0600); err != nil {
					t.Fatal(err)
				}
				_, err = rotateHAGroupMasterKeyWithPressureFactory(ctx, f.configs[:], target, "history-before-kill", nil, functionalHAGroupPressure)
			} else {
				o := kmsRotationOptions{operationID: "history-before-kill", factory: h.factory, random: bytes.NewReader(first), now: time.Now}
				_, err = rotateHAGroupKeyWithOptions(ctx, f.configs[:], "", o.operationID, nil, functionalHAGroupPressure, &o)
			}
			if err != nil {
				t.Fatal("first real complete group", err)
			}
			var original [2][32]byte
			for n, cfg := range f.configs {
				raw, err := os.ReadFile(haGroupRotationIntentPath(cfg.Storage.DataDir, 0))
				if err != nil {
					t.Fatal(err)
				}
				original[n] = sha256.Sum256(raw)
				clear(raw)
			}
			next := bytes.Repeat([]byte{113}, 32)
			if mode == "file" {
				if err := os.WriteFile(target, next, 0600); err != nil {
					t.Fatal(err)
				}
			}
			s := haGroupRotationCrashSpec{KMS: mode == "key_slots", Configs: f.configs, Target: target, Operation: "history-second-kill", Phase: "member_0_file_3", Marker: filepath.Join(dir, "marker")}
			raw, err := json.Marshal(s)
			if err != nil {
				t.Fatal(err)
			}
			spec := filepath.Join(dir, "spec.json")
			if err := os.WriteFile(spec, raw, 0600); err != nil {
				t.Fatal(err)
			}
			clear(raw)
			executable, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(executable, "-test.run", "^TestHAGroupRotationSIGKILLChild$", "-test.timeout=90s")
			cmd.Env = append(os.Environ(), "HALRO_GROUP_ROTATION_CHILD="+spec)
			output, err := cmd.CombinedOutput()
			var exit *exec.ExitError
			if !errors.As(err, &exit) {
				t.Fatalf("no SIGKILL: %v %s", err, output)
			}
			status, ok := exit.Sys().(syscall.WaitStatus)
			if !ok || !status.Signaled() || status.Signal() != syscall.SIGKILL {
				t.Fatalf("wrong termination: %v %s", err, output)
			}
			if marker, err := os.ReadFile(s.Marker); err != nil || string(marker) != s.Phase {
				t.Fatal("wrong actual history boundary", err)
			}
			var result HAGroupKeyRotationResult
			if mode == "file" {
				result, err = rotateHAGroupMasterKeyWithPressureFactory(ctx, f.configs[:], target, s.Operation, nil, functionalHAGroupPressure)
			} else {
				h = newKMSAppHarness(t)
				o := kmsRotationOptions{operationID: s.Operation, factory: h.factory, random: bytes.NewReader(nil), now: time.Now}
				result, err = rotateHAGroupKeyWithOptions(ctx, f.configs[:], "", s.Operation, nil, functionalHAGroupPressure, &o)
				if groupKMSWrapCalls(h) != 0 {
					t.Fatal("fresh resume generated/wrapped a different DEK")
				}
			}
			if err != nil || !result.RecoveredPending || result.NewFingerprint != keyFingerprint(next) {
				t.Fatal("fresh history/unlock resume", result, err)
			}
			for n, cfg := range f.configs {
				if err := checkHAGroupKeyRotationPending(ctx, cfg, next, functionalHAMetadataManager(t, cfg)); err != nil {
					t.Fatal("actual completion/history gate", err)
				}
				raw, err := os.ReadFile(haGroupRotationIntentPath(cfg.Storage.DataDir, 0))
				if err != nil || sha256.Sum256(raw) != original[n] {
					t.Fatal("prior original capsule changed", err)
				}
				clear(raw)
			}
		})
	}
}
