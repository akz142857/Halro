package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/config"
)

func TestHAGroupRotationFourFixedHistoriesAndFifthRefusal(t *testing.T) {
	for _, mode := range []string{"file", "key_slots"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 150*time.Second)
			defer cancel()
			var fixture haMetadataMaintenanceFixture
			var owners []*haSelectedRuntimeFoundation
			var harness *kmsAppHarness
			if mode == "file" {
				fixture, owners = prepareHAGroupRotationColdMembers(t, ctx)
			} else {
				fixture, owners, harness = prepareHAGroupKMSColdMembers(t, ctx)
			}
			for _, o := range owners {
				if err := o.Close(); err != nil {
					t.Fatal(err)
				}
				if err := o.owners.Components.Guard.Close(); err != nil {
					t.Fatal(err)
				}
			}
			var capsules [2][haGroupRotationIntentSlots][32]byte
			current := bytes.Clone(fixture.keys.Master)
			defer clear(current)
			dir := t.TempDir()
			for cycle := 0; cycle < haGroupRotationIntentSlots; cycle++ {
				next := bytes.Repeat([]byte{byte(120 + cycle)}, 32)
				op := []string{"history-one", "history-two", "history-three", "history-four"}[cycle]
				var result HAGroupKeyRotationResult
				var err error
				if mode == "file" {
					target := filepath.Join(dir, op+".key")
					if err := os.WriteFile(target, next, 0600); err != nil {
						t.Fatal(err)
					}
					result, err = rotateHAGroupMasterKeyWithPressureFactory(ctx, fixture.configs[:], target, op, nil, functionalHAGroupPressure)
				} else {
					options := kmsRotationOptions{operationID: op, factory: harness.factory, random: bytes.NewReader(next), now: time.Now}
					result, err = rotateHAGroupKeyWithOptions(ctx, fixture.configs[:], "", op, nil, functionalHAGroupPressure, &options)
				}
				if err != nil {
					t.Fatal("actual complete group repeat", cycle, err)
				}
				if result.RecoveredPending || result.OldFingerprint != keyFingerprint(current) || result.NewFingerprint != keyFingerprint(next) || result.NewKeyVersion != result.OldKeyVersion+1 {
					t.Fatal(result)
				}
				for n, cfg := range fixture.configs {
					raw, err := os.ReadFile(haGroupHistoryPath(cfg))
					if err != nil {
						t.Fatal(err)
					}
					keys, err := decodeHAGroupHistory(raw, next, cfg)
					clear(raw)
					if err != nil {
						t.Fatal(err)
					}
					count := 0
					for _, e := range keys {
						if e != (haGroupHistoricalKey{}) {
							count++
						}
					}
					if count != cycle {
						t.Fatal("history lost or unbounded", count, cycle)
					}
					clearHAGroupHistoricalKeys(&keys)
					if err := checkHAGroupKeyRotationPending(ctx, cfg, next, functionalHAMetadataManager(t, cfg)); err != nil {
						t.Fatal("ordinary current-key history gate", cycle, n, err)
					}
					for slot := 0; slot <= cycle; slot++ {
						raw, err := os.ReadFile(haGroupRotationIntentPath(cfg.Storage.DataDir, slot))
						if err != nil {
							t.Fatal(err)
						}
						digest := sha256.Sum256(raw)
						clear(raw)
						if slot == cycle {
							capsules[n][slot] = digest
						} else if capsules[n][slot] != digest {
							t.Fatal("old original capsule rewritten/deleted", slot)
						}
					}
					actual, key, err := readMemberState(cfg, next)
					clear(key[:])
					if err != nil || actual.DurableIndex != actual.ConfirmedIndex || actual.DurableIndex != actual.AppliedIndex {
						t.Fatal(actual, err)
					}
				}
				clear(current)
				current = bytes.Clone(next)
			}
			var before [2][]byte
			for n, cfg := range fixture.configs {
				var err error
				before[n], err = os.ReadFile(haGroupKeyDestination(cfg))
				if err != nil {
					t.Fatal(err)
				}
			}
			var err error
			if mode == "file" {
				target := filepath.Join(dir, "fifth.key")
				if err := os.WriteFile(target, bytes.Repeat([]byte{124}, 32), 0600); err != nil {
					t.Fatal(err)
				}
				_, err = rotateHAGroupMasterKeyWithPressureFactory(ctx, fixture.configs[:], target, "history-five", nil, functionalHAGroupPressure)
			} else {
				wraps := groupKMSWrapCalls(harness)
				options := kmsRotationOptions{operationID: "history-five", factory: harness.factory, random: bytes.NewReader(bytes.Repeat([]byte{124}, 32)), now: time.Now}
				_, err = rotateHAGroupKeyWithOptions(ctx, fixture.configs[:], "", "history-five", nil, functionalHAGroupPressure, &options)
				if groupKMSWrapCalls(harness) != wraps {
					t.Fatal("fifth operation wrapped upstream after finite history exhausted")
				}
			}
			if !errors.Is(err, authenticatedstate.ErrLimit) {
				t.Fatal("fifth history was not refused before mutation", err)
			}
			for n, cfg := range fixture.configs {
				raw, err := os.ReadFile(haGroupKeyDestination(cfg))
				if err != nil || !bytes.Equal(raw, before[n]) {
					t.Fatal("fifth refusal replaced current authority", err)
				}
				clear(raw)
				clear(before[n])
			}
			if mode == config.MasterKeyModeKeySlots {
				key, p, err := unlockHAMemoryCandidate(ctx, fixture.configs[0], functionalHAMetadataManager(t, fixture.configs[0]), harness.factory)
				if err != nil || !bytes.Equal(key, current) {
					t.Fatal(err)
				}
				clear(key)
				if err := p.Close(); err != nil {
					t.Fatal(err)
				}
			}
			proveHAGroupHistoryRejection(t, ctx, fixture.configs[0], current)
			for _, cfg := range fixture.configs {
				if err := os.Rename(cfg.MetadataPath(), cfg.MetadataPath()+".preserved-group-history"); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(cfg.MetadataPath(), 0700); err != nil {
					t.Fatal(err)
				}
			}
			fixture.keys.Master = bytes.Clone(current)
			defer clear(fixture.keys.Master)
			proveDefaultHASelectedRuntime(t, ctx, fixture, [2]*haSelectedRuntimeFoundation{owners[0], owners[1]})
		})
	}
}

func proveHAGroupHistoryRejection(t *testing.T, ctx context.Context, cfg config.Config, key []byte) {
	t.Helper()
	path := haGroupHistoryPath(cfg)
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(original)
	entries, err := decodeHAGroupHistory(original, key, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer clearHAGroupHistoricalKeys(&entries)
	for _, kind := range []string{"foreign-node", "wrong-capsule", "incomplete-entry", "oversize"} {
		t.Run(kind, func(t *testing.T) {
			badCfg, badEntries := cfg, entries
			defer clearHAGroupHistoricalKeys(&badEntries)
			if kind == "foreign-node" {
				peer := *cfg.Replication
				peer.NodeID = "foreign-history-node"
				badCfg.Replication = &peer
			} else if kind == "wrong-capsule" {
				badEntries[0].Raw[0] ^= 1
			} else if kind == "incomplete-entry" {
				badEntries[0].Done = [32]byte{}
			}
			raw, err := encodeHAGroupHistory(badCfg, key, badEntries)
			if err != nil {
				t.Fatal(err)
			}
			if kind == "oversize" {
				clear(raw)
				raw = bytes.Repeat([]byte{42}, haGroupHistoryLimit+1)
			}
			defer clear(raw)
			if err := os.WriteFile(path, raw, 0600); err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := os.WriteFile(path, original, 0600); err != nil {
					t.Error(err)
				}
			}()
			if err := checkHAGroupKeyRotationPending(ctx, cfg, key, functionalHAMetadataManager(t, cfg)); err == nil {
				t.Fatal("key-only history tamper bypassed completion gate")
			}
			actual, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(actual, raw) {
				t.Fatal("foreign history rewritten during refusal", err)
			}
			clear(actual)
		})
	}
	marker := haGroupRotationIntentPath(cfg.Storage.DataDir, 0) + ".complete"
	preserved := marker + ".preserved"
	if err := os.Rename(marker, preserved); err != nil {
		t.Fatal(err)
	}
	if err := checkHAGroupKeyRotationPending(ctx, cfg, key, functionalHAMetadataManager(t, cfg)); err == nil {
		t.Fatal("historical key granted completion without original marker")
	}
	if err := os.Rename(preserved, marker); err != nil {
		t.Fatal(err)
	}
	if err := checkHAGroupKeyRotationPending(ctx, cfg, key, functionalHAMetadataManager(t, cfg)); err != nil {
		t.Fatal("restored original history/marker did not qualify", err)
	}
}
