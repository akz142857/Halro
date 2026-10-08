package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/durable"
	corekms "github.com/akz142857/Halro/internal/kms"
)

type haGroupRotationCrashSpec struct {
	KMS                              bool
	Configs                          [2]config.Config
	Target, Operation, Phase, Marker string
}

func TestHAGroupRotationSIGKILLChild(t *testing.T) {
	path := os.Getenv("HALRO_GROUP_ROTATION_CHILD")
	if path == "" {
		t.Skip("private actual group rotation child")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var s haGroupRotationCrashSpec
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatal(err)
	}
	clear(raw)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	phase := func(phase string) error {
		if phase != s.Phase {
			return nil
		}
		f, err := os.OpenFile(s.Marker, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			return err
		}
		_, writeErr := f.WriteString(phase)
		err = errors.Join(writeErr, f.Sync(), f.Close())
		if err != nil {
			return err
		}
		if err := durable.SyncDirectory(filepath.Dir(s.Marker)); err != nil {
			return err
		}
		if err := syscall.Kill(os.Getpid(), syscall.SIGKILL); err != nil {
			return err
		}
		for {
			time.Sleep(time.Hour)
		}
	}
	if s.KMS {
		h := newKMSAppHarness(t)
		options := kmsRotationOptions{operationID: s.Operation, factory: h.factory, random: bytes.NewReader(bytes.Repeat([]byte{113}, 32)), now: time.Now}
		_, err = rotateHAGroupKeyWithOptions(ctx, s.Configs[:], "", s.Operation, phase, functionalHAGroupPressure, &options)
	} else {
		_, err = rotateHAGroupMasterKeyWithPressureFactory(ctx, s.Configs[:], s.Target, s.Operation, phase, functionalHAGroupPressure)
	}
	t.Fatal("actual phase was not SIGKILL", err)
}

func TestHAGroupRotationActualSIGKILLMixedKeyResume(t *testing.T) {
	for _, phase := range []string{"member_0_intent", "intents_durable", "group_confirmed", "member_0_common", "member_0_file_0", "member_0_file_2"} {
		t.Run(phase, func(t *testing.T) {
			stressPhase(t, phase, "member_0_intent", "group_confirmed", "member_0_file_2")
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Second)
			defer cancel()
			fixture, members := prepareHAGroupRotationColdMembers(t, ctx)
			for n, m := range members {
				if err := m.Close(); err != nil {
					t.Fatal(n, err)
				}
				if err := m.owners.Components.Guard.Close(); err != nil {
					t.Fatal(n, err)
				}
			}
			dir := t.TempDir()
			target := filepath.Join(dir, "new.key")
			next := bytes.Repeat([]byte{97}, 32)
			if err := os.WriteFile(target, next, 0600); err != nil {
				t.Fatal(err)
			}
			s := haGroupRotationCrashSpec{Configs: fixture.configs, Target: target, Operation: "group-real-kill", Phase: phase, Marker: filepath.Join(dir, "phase")}
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
				t.Fatalf("no actual SIGKILL: %v %s", err, output)
			}
			status, ok := exit.Sys().(syscall.WaitStatus)
			if !ok || !status.Signaled() || status.Signal() != syscall.SIGKILL {
				t.Fatalf("wrong termination: %v %s", err, output)
			}
			if marker, err := os.ReadFile(s.Marker); err != nil || string(marker) != phase {
				t.Fatal("wrong durable boundary", err)
			}
			for n, cfg := range fixture.configs {
				if phase == "member_0_intent" && n > 0 {
					if _, key, err := readMemberState(cfg, fixture.keys.Master); err != nil {
						t.Fatal(err)
					} else {
						clear(key[:])
					}
					continue
				}
				for _, key := range [][]byte{fixture.keys.Master, next} {
					if err := checkHAGroupKeyRotationPending(ctx, cfg, key, functionalHAMetadataManager(t, cfg)); !errors.Is(err, ErrHAGroupKeyRotationPending) {
						t.Fatal("ordinary Runtime pending gate accepted", phase, err)
					}
				}
			}
			result, err := rotateHAGroupMasterKeyWithPressureFactory(ctx, fixture.configs[:], target, s.Operation, nil, functionalHAGroupPressure)
			if err != nil {
				t.Fatal("fresh complete group actual resume", phase, err)
			}
			if !result.RecoveredPending || result.NewFingerprint != keyFingerprint(next) {
				t.Fatal(result)
			}
			for _, cfg := range fixture.configs {
				actual, err := os.ReadFile(cfg.Storage.MasterKey.File)
				if err != nil || !bytes.Equal(actual, next) {
					t.Fatal("real File not switched", err)
				}
				clear(actual)
				if err := checkHAGroupKeyRotationPending(ctx, cfg, next, functionalHAMetadataManager(t, cfg)); err != nil {
					t.Fatal("completed actual group pending gate", err)
				}
				if _, key, err := readMemberState(cfg, fixture.keys.Master); err == nil {
					clear(key[:])
					t.Fatal("old key reopened target member")
				}
			}
		})
	}
}

func TestHAGroupKMSRotationActualSIGKILLMixedUnlockResume(t *testing.T) {
	for _, phase := range []string{"member_0_intent", "metadata_durable", "member_0_file_2"} {
		t.Run(phase, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Second)
			defer cancel()
			fixture, owners, h := prepareHAGroupKMSColdMembers(t, ctx)
			for _, o := range owners {
				if err := o.Close(); err != nil {
					t.Fatal(err)
				}
				if err := o.owners.Components.Guard.Close(); err != nil {
					t.Fatal(err)
				}
			}
			dir := t.TempDir()
			s := haGroupRotationCrashSpec{KMS: true, Configs: fixture.configs, Operation: "kms-group-real-kill", Phase: phase, Marker: filepath.Join(dir, "phase")}
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
				t.Fatalf("no SIGKILL %v %s", err, output)
			}
			status, ok := exit.Sys().(syscall.WaitStatus)
			if !ok || !status.Signaled() || status.Signal() != syscall.SIGKILL {
				t.Fatalf("wrong termination %v %s", err, output)
			}
			if marker, err := os.ReadFile(s.Marker); err != nil || string(marker) != phase {
				t.Fatal(err)
			}
			h = newKMSAppHarness(t)
			// A fresh fake service has only its fixed wrapping keys; it receives no DEK
			// from the terminated process. The actual node carriers supply candidates.
			options := kmsRotationOptions{operationID: s.Operation, factory: h.factory, random: bytes.NewReader(nil), now: time.Now}
			result, err := rotateHAGroupKeyWithOptions(ctx, fixture.configs[:], "", s.Operation, nil, functionalHAGroupPressure, &options)
			if err != nil {
				t.Fatal("fresh genuine group KMS resume", err)
			}
			next := bytes.Repeat([]byte{113}, 32)
			if !result.RecoveredPending || result.NewFingerprint != keyFingerprint(next) {
				t.Fatal(result)
			}
			for _, cfg := range fixture.configs {
				key, p, err := unlockHAMemoryCandidate(ctx, cfg, functionalHAMetadataManager(t, cfg), h.factory)
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(key, next) {
					t.Fatal("wrong target KMS candidate")
				}
				clear(key)
				if err := p.Close(); err != nil {
					t.Fatal(err)
				}
			}
			for _, w := range h.wrappers {
				for _, call := range w.Calls() {
					if call.Operation == corekms.OperationWrap {
						t.Fatal("resume wrapped a newly generated DEK")
					}
				}
			}
		})
	}
}
