package metadatastate

import (
	"bytes"
	"context"
	"crypto/hkdf"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/backup"
	"github.com/akz142857/Halro/internal/buildinfo"
	"github.com/akz142857/Halro/internal/resourcebudget"
	boltstore "github.com/akz142857/Halro/internal/store/bolt"
	bbolt "go.etcd.io/bbolt"
)

func baselineBudget(t *testing.T) *resourcebudget.Manager {
	t.Helper()
	l := resourcebudget.Limits{Total: resourcebudget.Cost{RAM: 256 << 20, Disk: 256 << 20}, MaxClaims: 32, MaxOwners: 4, MaxPressureAge: time.Hour}
	for i := range l.Classes {
		l.Classes[i] = l.Total
	}
	l.Classes[resourcebudget.Recovery] = resourcebudget.Cost{}
	l.Classes[resourcebudget.Control] = resourcebudget.Cost{RAM: 32 * resourcebudget.ClaimRAMBytes}
	m, err := resourcebudget.New(l)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.SetPressure(resourcebudget.Healthy); err != nil {
		t.Fatal(err)
	}
	return m
}

func baselineOptions(t *testing.T) BaselineOptions {
	return BaselineOptions{Identity: sha256.Sum256([]byte("independent-instance-member-component-identity")), InstanceID: "ins_baseline_test", KeyGeneration: 2, Budget: baselineBudget(t), MaxSourceBytes: 64 << 20, MaxDiskBytes: 64 << 20, MaxAppendBytes: 8 << 20}
}

func privateSource(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "source")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(dir, "metadata.db")
}

func baselineArchive(t *testing.T, paged bool, mutate func(*bbolt.Tx) error, alterManifest func(*backup.CreateOptions)) (*backup.VerifiedArchive, string, []authenticatedstate.Row) {
	t.Helper()
	dir := t.TempDir()
	metadata := filepath.Join(dir, "metadata.db")
	s, err := boltstore.Open(metadata)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := bbolt.Open(metadata, 0600, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Update(func(tx *bbolt.Tx) error {
		for _, name := range []string{"projects", "admin_sessions", "usage_daily_rollup", "credentials"} {
			if err := tx.Bucket([]byte(name)).Put([]byte("fixture"), []byte("raw-"+name)); err != nil {
				return err
			}
		}
		meta := tx.Bucket([]byte("meta"))
		if err := meta.Put([]byte("instance_id"), []byte("ins_baseline_test")); err != nil {
			return err
		}
		for _, name := range []string{"vault_keyring", "ledger_hmac_envelope", "metadata_hmac_envelope", "shutdown_truncated_attempts_total"} {
			if err := meta.Put([]byte(name), []byte("raw-"+name)); err != nil {
				return err
			}
		}
		if err := meta.Put([]byte("metadata_journal_epoch"), binary.BigEndian.AppendUint64(nil, 7)); err != nil {
			return err
		}
		if err := meta.Put([]byte("applied_journal_sequence"), binary.BigEndian.AppendUint64(nil, 13)); err != nil {
			return err
		}
		root := tx.Bucket([]byte("deployment_price_timeline"))
		nested, err := root.CreateBucket([]byte{'p', 0, '/'})
		if err != nil {
			return err
		}
		if err := nested.SetSequence(17); err != nil {
			return err
		}
		for n := 0; n < 130; n++ {
			if err := nested.Put([]byte(fmt.Sprintf("key-%03d", n)), []byte(fmt.Sprintf("price-%03d", n))); err != nil {
				return err
			}
		}
		if err := nested.Put([]byte{0, 'k'}, []byte{}); err != nil {
			return err
		}
		if _, err := root.CreateBucket([]byte("empty")); err != nil {
			return err
		}
		if mutate != nil {
			return mutate(tx)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// Independent recursive oracle in tests only. Production uses a fixed stack.
	var expected []authenticatedstate.Row
	var collect func(*bbolt.Bucket, [][]byte) error
	collect = func(b *bbolt.Bucket, path [][]byte) error {
		key, err := RowKey(path, nil, true)
		if err != nil {
			return err
		}
		expected = append(expected, authenticatedstate.Row{Key: key, Value: binary.BigEndian.AppendUint64(nil, b.Sequence())})
		return b.ForEach(func(k, v []byte) error {
			if child := b.Bucket(k); child != nil {
				return collect(child, append(append([][]byte{}, path...), bytes.Clone(k)))
			}
			key, err := RowKey(path, k, false)
			if err != nil {
				return err
			}
			expected = append(expected, authenticatedstate.Row{Key: key, Value: bytes.Clone(v)})
			return nil
		})
	}
	if err := db.View(func(tx *bbolt.Tx) error {
		return tx.ForEach(func(k []byte, b *bbolt.Bucket) error { return collect(b, [][]byte{bytes.Clone(k)}) })
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = boltstore.OpenReadOnly(metadata)
	if err != nil {
		t.Fatal(err)
	}
	info, err := s.Info()
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	small := filepath.Join(dir, "native")
	if err := os.WriteFile(small, []byte("authenticated native backup bytes"), 0600); err != nil {
		t.Fatal(err)
	}
	o := backup.CreateOptions{OutputPath: filepath.Join(dir, "source.hmbk"), BackupKey: bytes.Repeat([]byte{31}, 32), Metadata: info, LedgerFeatureEpoch: 4, MinimumLedgerReaderVersion: "v4", MasterKeyFingerprint: "sha256:" + strings.Repeat("0", 64), PricingStateSHA256: "sha256:" + strings.Repeat("1", 64), PendingIntentSHA256: "sha256:" + strings.Repeat("2", 64), Build: buildinfo.Info{Version: "test"}, Files: []backup.SourceFile{{ArchivePath: "config/config.yaml", LocalPath: small}, {ArchivePath: "data/metadata.db", LocalPath: metadata}, {ArchivePath: "data/ledger/ledger.wal", LocalPath: small}, {ArchivePath: "data/audit/audit.log", LocalPath: small}}}
	if alterManifest != nil {
		alterManifest(&o)
	}
	if paged {
		_, err = backup.CreatePaged(o)
	} else {
		_, err = backup.Create(o)
	}
	if err != nil {
		t.Fatal(err)
	}
	h, err := backup.OpenVerified(context.Background(), o.OutputPath, o.BackupKey)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := h.Close(); err != nil {
			t.Error(err)
		}
	})
	return h, metadata, expected
}

func readBaselineRows(t *testing.T, b *Baseline) []authenticatedstate.Row {
	t.Helper()
	p, err := b.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	var rows []authenticatedstate.Row
	cursor := ""
	for {
		page, err := p.Range(context.Background(), authenticatedstate.RangeOptions{Cursor: cursor, MaxRows: 17, MaxBytes: 1 << 20, MaxPages: 4096})
		if err != nil {
			t.Fatal(err)
		}
		rows = append(rows, page.Rows...)
		if page.Done {
			break
		}
		cursor = page.Cursor
	}
	filtered := rows[:0]
	for _, row := range rows {
		if string(row.Key) != string(baselineEvidenceKey) {
			filtered = append(filtered, row)
		}
	}
	return filtered
}

func TestBaselineBackupBuildCommitsAllClassesNestedEmptyBucketsAndSequence(t *testing.T) {
	for _, paged := range []bool{false, true} {
		t.Run(fmt.Sprint(paged), func(t *testing.T) {
			h, live, expected := baselineArchive(t, paged, nil, nil)
			o := baselineOptions(t)
			source, out := privateSource(t), filepath.Join(t.TempDir(), "root")
			// Later live DB mutation is irrelevant: it is neither read nor signed.
			db, err := bbolt.Open(live, 0600, nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := db.Update(func(tx *bbolt.Tx) error {
				return tx.Bucket([]byte("projects")).Put([]byte("fixture"), []byte("live-tamper"))
			}); err != nil {
				t.Fatal(err)
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			b, err := BuildFromBackup(context.Background(), h, bytes.Repeat([]byte{51}, 32), source, out, o)
			if err != nil || b == nil {
				t.Fatal(b, err)
			}
			t.Cleanup(func() {
				if err := b.Discard(); err != nil {
					t.Error(err)
				}
			})
			if err := h.Close(); err != nil {
				t.Fatal(err)
			}
			if actual := readBaselineRows(t, b); !reflect.DeepEqual(actual, expected) {
				t.Fatalf("complete content differs: actual=%d expected=%d", len(actual), len(expected))
			}
			e := b.Evidence()
			root, err := b.store.RootIdentity(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if e.Rows != root.Rows || e.Rows != uint64(len(expected)+1) || e.Metadata.MetadataJournalEpoch != 7 || e.Metadata.MetadataJournalSequence != 13 || e.File.Path != "data/metadata.db" || e.CiphertextDigest == ([32]byte{}) || e.CatalogDigest != boltstore.MetadataBaselineCatalogDigest() {
				t.Fatal(e)
			}
			p, err := b.Snapshot(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			persisted, err := ReadBaselineEvidence(context.Background(), p)
			if err != nil || persisted != e {
				t.Fatalf("authenticated provenance receipt differs: %#v %v", persisted, err)
			}
			key, err := RowKey([][]byte{[]byte("projects")}, []byte("absent"), false)
			if err != nil {
				t.Fatal(err)
			}
			if _, found, err := p.Get(context.Background(), key); err != nil || found {
				t.Fatal("absence not proved", found, err)
			}
			if err := p.Close(); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(source); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("source retained after ready", err)
			}
			if o.Budget.Snapshot().Classes[resourcebudget.IO] != (resourcebudget.Cost{}) {
				t.Fatal("source mapping fee leaked", o.Budget.Snapshot())
			}
			if err := b.Close(); err != nil {
				t.Fatal(err)
			}
			if o.Budget.Snapshot().Used.Disk != o.MaxDiskBytes {
				t.Fatal("closed root refunded persistent owner", o.Budget.Snapshot())
			}
			if err := b.Discard(); err != nil {
				t.Fatal(err)
			}
			if o.Budget.Snapshot().Used != (resourcebudget.Cost{}) {
				t.Fatal(o.Budget.Snapshot())
			}
		})
	}
}

func TestBaselineRefusesUnknownCoverageAndInconsistentManifestWithoutReturningRoot(t *testing.T) {
	for _, shape := range []string{"unknown bucket", "unknown meta key", "nested meta", "wrong instance", "wrong txid", "wrong epoch", "malformed sequence", "oversized value"} {
		t.Run(shape, func(t *testing.T) {
			mutate := func(tx *bbolt.Tx) error {
				switch shape {
				case "unknown bucket":
					_, err := tx.CreateBucket([]byte("new-unclassified"))
					return err
				case "unknown meta key":
					return tx.Bucket([]byte("meta")).Put([]byte("new-unclassified"), []byte("security critical"))
				case "nested meta":
					_, err := tx.Bucket([]byte("meta")).CreateBucket([]byte("nested"))
					return err
				case "malformed sequence":
					return tx.Bucket([]byte("meta")).Put([]byte("applied_journal_sequence"), []byte{1})
				case "oversized value":
					return tx.Bucket([]byte("projects")).Put([]byte("large"), make([]byte, authenticatedstate.MaxValueBytes+1))
				}
				return nil
			}
			alter := func(o *backup.CreateOptions) {
				if shape == "wrong txid" {
					o.Metadata.TxID++
				}
				if shape == "wrong epoch" {
					o.Metadata.MetadataJournalEpoch++
				}
			}
			h, _, _ := baselineArchive(t, true, mutate, alter)
			o := baselineOptions(t)
			if shape == "wrong instance" {
				o.InstanceID = "ins_other"
			}
			source, out := privateSource(t), filepath.Join(t.TempDir(), "root")
			b, err := BuildFromBackup(context.Background(), h, bytes.Repeat([]byte{51}, 32), source, out, o)
			if err == nil || b != nil {
				t.Fatal("ineligible source returned root", b, err)
			}
			if _, err := os.Stat(source); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("source leaked", err)
			}
			if _, err := os.Stat(out); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("failed root leaked", err)
			}
			if o.Budget.Snapshot().Used != (resourcebudget.Cost{}) || o.Budget.Snapshot().Owners != 0 {
				t.Fatal(o.Budget.Snapshot())
			}
		})
	}
}

func TestBaselineIndependentIdentityKeyGenerationAndBackupCutBindSavedRoot(t *testing.T) {
	h, _, _ := baselineArchive(t, true, nil, nil)
	o := baselineOptions(t)
	ledgerKey := bytes.Repeat([]byte{51}, 32)
	dir := filepath.Join(t.TempDir(), "root")
	b, err := BuildFromBackup(context.Background(), h, ledgerKey, privateSource(t), dir, o)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Discard()
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	credit, err := b.store.DiskReservation()
	if err != nil {
		t.Fatal(err)
	}
	for _, shape := range []string{"same", "identity", "key generation", "backup id", "file digest", "ledger key"} {
		t.Run(shape, func(t *testing.T) {
			wanted, e, key := o, b.Evidence(), bytes.Clone(ledgerKey)
			switch shape {
			case "identity":
				wanted.Identity[0] ^= 1
			case "key generation":
				wanted.KeyGeneration++
			case "backup id":
				e.BackupID += "other"
			case "file digest":
				e.File.SHA256 = strings.Repeat("0", 64)
			case "ledger key":
				key[0] ^= 1
			}
			scope := baselineScope(wanted)
			mac, err := hkdf.Key(sha256.New, key, scope[:], "halro:metadata:authenticated-baseline:hmac:v1", 32)
			if err != nil {
				t.Fatal(err)
			}
			defer clear(mac)
			cut, err := baselineCut(scope, e)
			if err != nil {
				t.Fatal(err)
			}
			before := o.Budget.Snapshot()
			s, err := authenticatedstate.OpenStore(context.Background(), dir, authenticatedstate.StoreOptions{Scope: scope, ExpectedCut: cut, Key: mac, MaxDiskBytes: o.MaxDiskBytes, MaxAppendBytes: o.MaxAppendBytes, Budget: o.Budget, DiskReservation: credit})
			if shape == "same" {
				if err != nil || s == nil {
					t.Fatal(s, err)
				}
				if err := s.Close(); err != nil {
					t.Fatal(err)
				}
			} else if err == nil || s != nil {
				t.Fatal("mixed identity/cut/key accepted", s, err)
			}
			if o.Budget.Snapshot().Used != before.Used || o.Budget.Snapshot().Owners != before.Owners {
				t.Fatal("borrowed opener changed persistent fees", before, o.Budget.Snapshot())
			}
		})
	}
}

func TestBaselineRowCodecSeparatesBinaryNamesLeavesAndBucketMarkers(t *testing.T) {
	var keys [][]byte
	for _, path := range [][][]byte{{[]byte("a")}, {[]byte("a"), []byte{0}}, {[]byte("a"), []byte{0, '/'}}, {[]byte("a"), []byte("a")}, {[]byte("a/a")}} {
		k, err := RowKey(path, nil, true)
		if err != nil {
			t.Fatal(err)
		}
		keys = append(keys, k)
		k, err = RowKey(path, []byte{0, 'k'}, false)
		if err != nil {
			t.Fatal(err)
		}
		keys = append(keys, k)
	}
	seen := map[string]bool{}
	for _, k := range keys {
		if seen[string(k)] {
			t.Fatal("ambiguous encoding", k)
		}
		seen[string(k)] = true
	}
	marker, _ := RowKey([][]byte{[]byte("a")}, nil, true)
	child, _ := RowKey([][]byte{[]byte("a"), []byte{0}}, nil, true)
	if bytes.Compare(marker, child) >= 0 {
		t.Fatal("bucket did not sort before binary child")
	}
	if _, err := RowKey([][]byte{[]byte("a")}, make([]byte, authenticatedstate.MaxKeyBytes), false); !errors.Is(err, authenticatedstate.ErrLimit) {
		t.Fatal(err)
	}
	if _, err := RowKey(make([][]byte, maxBaselineDepth+1), nil, true); !errors.Is(err, authenticatedstate.ErrLimit) {
		t.Fatal(err)
	}
}

func TestBaselineAdmissionFailurePreservesForeignFilesAndRetiresOwnedSource(t *testing.T) {
	h, _, _ := baselineArchive(t, true, nil, nil)
	for _, shape := range []string{"nil archive", "nil budget", "zero identity", "wrong key", "key generation", "instance id", "relative output", "source limit", "pressure", "source RAM", "root disk", "existing root", "canceled"} {
		t.Run(shape, func(t *testing.T) {
			o := baselineOptions(t)
			manager := o.Budget
			key, archive := bytes.Repeat([]byte{51}, 32), h
			source, out := privateSource(t), filepath.Join(t.TempDir(), "root")
			ctx := context.Background()
			switch shape {
			case "nil archive":
				archive = nil
			case "nil budget":
				o.Budget = nil
			case "zero identity":
				o.Identity = [32]byte{}
			case "wrong key":
				key = nil
			case "key generation":
				o.KeyGeneration = 0
			case "instance id":
				o.InstanceID = ""
			case "relative output":
				out = "metadata-baseline"
			case "source limit":
				o.MaxSourceBytes = 1
			case "pressure":
				if err := manager.SetPressure(resourcebudget.Pressured); err != nil {
					t.Fatal(err)
				}
			case "source RAM":
				fee, err := manager.TryAcquire(resourcebudget.IO, resourcebudget.Cost{RAM: 240 << 20})
				if err != nil {
					t.Fatal(err)
				}
				defer fee.Release()
			case "root disk":
				fee, err := manager.TryAcquire(resourcebudget.Background, resourcebudget.Cost{Disk: 224 << 20})
				if err != nil {
					t.Fatal(err)
				}
				defer fee.Release()
			case "existing root":
				if err := os.Mkdir(out, 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(out, "foreign"), []byte("preserved"), 0600); err != nil {
					t.Fatal(err)
				}
			case "canceled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			before := manager.Snapshot()
			b, err := BuildFromBackup(ctx, archive, key, source, out, o)
			if err == nil || b != nil {
				t.Fatal("invalid request returned usable baseline", b, err)
			}
			if _, err := os.Stat(source); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("source leaked", err)
			}
			if shape == "existing root" {
				raw, err := os.ReadFile(filepath.Join(out, "foreign"))
				if err != nil || string(raw) != "preserved" {
					t.Fatal(string(raw), err)
				}
			} else if filepath.IsAbs(out) {
				if _, err := os.Stat(out); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("failed output leaked", err)
				}
			}
			if manager.Snapshot().Used != before.Used || manager.Snapshot().Owners != before.Owners {
				t.Fatal("failed candidate refunded unrelated fee or leaked owner", before, manager.Snapshot())
			}
		})
	}
}

func TestBaselineTamperedContentFailsAuthenticatedReadWithoutFalseAbsence(t *testing.T) {
	h, _, _ := baselineArchive(t, true, nil, nil)
	o := baselineOptions(t)
	dir := filepath.Join(t.TempDir(), "root")
	b, err := BuildFromBackup(context.Background(), h, bytes.Repeat([]byte{51}, 32), privateSource(t), dir, o)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Discard()
	pin, err := b.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer pin.Close()
	file := filepath.Join(dir, "pages")
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	position := bytes.Index(raw, []byte("raw-projects"))
	if position < 0 {
		t.Fatal("fixture missing actual project payload")
	}
	f, err := os.OpenFile(file, os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	_, writeErr := f.WriteAt([]byte{'X'}, int64(position))
	if err := errors.Join(writeErr, f.Sync(), f.Close()); err != nil {
		t.Fatal(err)
	}
	key, err := RowKey([][]byte{[]byte("projects")}, []byte("fixture"), false)
	if err != nil {
		t.Fatal(err)
	}
	if _, found, err := pin.Get(context.Background(), key); !errors.Is(err, authenticatedstate.ErrCorrupt) || found {
		t.Fatal("tampered content became value or false absence", found, err)
	}
	if _, err := b.Snapshot(context.Background()); err == nil {
		t.Fatal("corrupt component remained readable")
	}
}
