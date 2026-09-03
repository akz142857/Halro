package app

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/config"
	corekms "github.com/akz142857/Halro/internal/kms"
	"github.com/akz142857/Halro/internal/ledger"
	boltstore "github.com/akz142857/Halro/internal/store/bolt"
)

func offlineUsageCommands() map[string]func(config.Config) error {
	return map[string]func(config.Config) error{
		"rebuild-summary": func(cfg config.Config) error { _, err := RebuildUsageSummary(context.Background(), cfg); return err },
		"compact":         func(cfg config.Config) error { _, err := CompactUsage(context.Background(), cfg); return err },
		"verify":          func(cfg config.Config) error { _, err := VerifyUsage(context.Background(), cfg); return err },
		"prune": func(cfg config.Config) error {
			_, err := PruneUsage(context.Background(), cfg, time.Now().AddDate(1, 0, 0))
			return err
		},
	}
}

// Alter a real event while keeping valid JSON and a valid CRC. A checksum-only
// reader accepts it; only the chain MAC proves it is no longer the billed event.
func forgeUsageFrame(t *testing.T, path string) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	const header = 24 + 32 + 32
	length := int(binary.BigEndian.Uint32(raw[16:20]))
	payload := raw[header : header+length]
	replaced := bytes.Replace(payload, []byte("project_1"), []byte("project_9"), 1)
	if bytes.Equal(payload, replaced) {
		t.Fatal("fixture has no project to forge")
	}
	copy(payload, replaced)
	crc := crc32.NewIEEE()
	crc.Write(raw[4:20])
	crc.Write(payload)
	binary.BigEndian.PutUint32(raw[20:24], crc.Sum32())
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestOfflineUsageRejectsUntrustedHistoryWithoutWriting(t *testing.T) {
	for _, scenario := range []string{"active_mac", "sealed_mac", "partial_tail", "checkpoint_truncation", "wrong_key", "missing_key", "old_schema"} {
		t.Run(scenario, func(t *testing.T) {
			cfg := testConfig(t)
			if err := Initialize(cfg); err != nil {
				t.Fatal(err)
			}
			runtime, err := Open(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
			if err != nil {
				t.Fatal(err)
			}
			billOneRequest(t, runtime, "auth_first")
			var sealedPath string
			if scenario == "sealed_mac" {
				roll, err := runtime.ledger.Roll()
				if err != nil {
					t.Fatal(err)
				}
				sealedPath = filepath.Join(filepath.Dir(cfg.LedgerPath()), roll.Sealed.File)
			}
			firstHead, _, _ := runtime.ledger.ChainHead()
			billOneRequest(t, runtime, "auth_second")
			if err := runtime.Close(); err != nil {
				t.Fatal(err)
			}
			// Put real existing derivatives on disk before attacking the authority.
			if _, err := CompactUsage(context.Background(), cfg); err != nil {
				t.Fatal(err)
			}
			switch scenario {
			case "active_mac":
				forgeUsageFrame(t, cfg.LedgerPath())
			case "sealed_mac":
				forgeUsageFrame(t, sealedPath)
			case "partial_tail":
				file, err := os.OpenFile(cfg.LedgerPath(), os.O_APPEND|os.O_WRONLY, 0)
				if err != nil {
					t.Fatal(err)
				}
				_, err = file.Write([]byte("torn"))
				if err := errors.Join(err, file.Close()); err != nil {
					t.Fatal(err)
				}
			case "checkpoint_truncation":
				if err := os.Truncate(cfg.LedgerPath(), firstHead.Offset); err != nil {
					t.Fatal(err)
				}
			case "wrong_key":
				if err := os.WriteFile(cfg.Storage.MasterKey.File, bytes.Repeat([]byte{0xee}, 32), 0o600); err != nil {
					t.Fatal(err)
				}
			case "missing_key":
				if err := os.Remove(cfg.Storage.MasterKey.File); err != nil {
					t.Fatal(err)
				}
			case "old_schema":
				raw, err := os.ReadFile(olderSchemaMetadata(t, boltstore.CurrentSchemaVersion()-2))
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(cfg.MetadataPath(), raw, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			before := doctorTree(t, cfg.Storage.DataDir)
			for name, command := range offlineUsageCommands() {
				t.Run(name, func(t *testing.T) {
					err := command(cfg)
					if err == nil {
						t.Fatal("untrusted history accepted")
					}
					if strings.HasSuffix(scenario, "_mac") && !errors.Is(err, ledger.ErrTampered) {
						t.Fatalf("expected MAC refusal: %v", err)
					}
					if scenario == "checkpoint_truncation" && !strings.Contains(err.Error(), "trusted checkpoint") {
						t.Fatalf("expected checkpoint refusal: %v", err)
					}
					if scenario == "old_schema" && (!errors.Is(err, boltstore.ErrSchemaVersionMismatch) || !strings.Contains(err.Error(), "old binary")) {
						t.Fatalf("missing upgrade advice: %v", err)
					}
					if after := doctorTree(t, cfg.Storage.DataDir); !reflect.DeepEqual(before, after) {
						t.Fatal("failed command changed the data directory")
					}
				})
			}
		})
	}
}

func TestOfflineUsageRebuildsSealedCompressedHistory(t *testing.T) {
	cfg := testConfig(t)
	if err := Initialize(cfg); err != nil {
		t.Fatal(err)
	}
	runtime, err := Open(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	billOneRequest(t, runtime, "auth_first")
	if _, err := runtime.ledger.Roll(); err != nil {
		t.Fatal(err)
	}
	billOneRequest(t, runtime, "auth_second")
	lastEvent, _, _ := runtime.ledger.ChainHead()
	if _, err := runtime.ledger.Roll(); err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.ledger.Compact(1); err != nil {
		t.Fatal(err)
	}
	if err := runtime.Close(); err != nil {
		t.Fatal(err)
	}
	// Rotation must keep using the wrapped ledger key, rather than deriving a
	// different key from the new master and refusing all previously billed work.
	newKeyFile := filepath.Join(t.TempDir(), "next-master.key")
	if err := os.WriteFile(newKeyFile, bytes.Repeat([]byte{0xaa}, 32), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := RotateMasterKey(context.Background(), cfg, newKeyFile); err != nil {
		t.Fatal(err)
	}
	before := allRollupRows(t, cfg.MetadataPath())
	report, err := RebuildUsageSummary(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	// Usage checkpoints name the last visited event, while authentication also
	// checks the current generation when a roll has left it empty.
	if report.Watermark != lastEvent {
		t.Fatalf("report=%+v", report)
	}
	if after := allRollupRows(t, cfg.MetadataPath()); len(before) == 0 || !reflect.DeepEqual(before, after) {
		t.Fatal("authenticated rebuild changed the totals")
	}
	if _, err := CompactUsage(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyUsage(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
}

func TestOfflineUsageAuthenticatesWithKMSAndRefusesUnavailableKey(t *testing.T) {
	cfg := kmsAppTestConfig(t)
	harness := newKMSAppHarness(t)
	previousFactory := defaultKMSWrapperFactory
	defaultKMSWrapperFactory = harness.factory
	t.Cleanup(func() { defaultKMSWrapperFactory = previousFactory })
	if err := initializeKMS(context.Background(), cfg, kmsInitializationOptions{
		factory: harness.factory, random: bytes.NewReader(bytes.Repeat([]byte{0x6d}, 32)),
	}); err != nil {
		t.Fatal(err)
	}
	runtime, err := Open(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	billOneRequest(t, runtime, "kms_usage")
	if err := runtime.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := RebuildUsageSummary(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	before := doctorTree(t, cfg.Storage.DataDir)
	defaultKMSWrapperFactory = func(context.Context, config.AllowedKMSKey) (corekms.Wrapper, error) {
		return nil, errors.New("test KMS unavailable")
	}
	for name, command := range offlineUsageCommands() {
		if err := command(cfg); err == nil {
			t.Fatalf("%s bypassed unavailable KMS", name)
		}
		if after := doctorTree(t, cfg.Storage.DataDir); !reflect.DeepEqual(before, after) {
			t.Fatalf("%s changed the directory", name)
		}
	}
}
