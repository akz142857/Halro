package ledger

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/resourcebudget"
	bbolt "go.etcd.io/bbolt"
)

func authenticatedLedgerTestOptions(t testing.TB) AuthenticatedLedgerOptions {
	t.Helper()
	limits := resourcebudget.Limits{Total: resourcebudget.Cost{RAM: 256 << 20, Disk: 256 << 20}, MaxClaims: 16, MaxOwners: 4, MaxPressureAge: time.Hour}
	for i := range limits.Classes {
		limits.Classes[i] = limits.Total
	}
	limits.Classes[resourcebudget.Recovery] = resourcebudget.Cost{}
	limits.Classes[resourcebudget.Control] = resourcebudget.Cost{RAM: 16 * resourcebudget.ClaimRAMBytes}
	budget, err := resourcebudget.New(limits)
	if err != nil {
		t.Fatal(err)
	}
	if err = budget.SetPressure(resourcebudget.Healthy); err != nil {
		t.Fatal(err)
	}
	return AuthenticatedLedgerOptions{Identity: sha256.Sum256([]byte("independent-test-ledger-identity")), MaxDiskBytes: 64 << 20, MaxAppendBytes: 8 << 20, Budget: budget}
}

// Compare all rows in both directions; recovery and governance indexes are
// included, not merely balances. Bolt is a freshly reconstructed reference.
func compareAuthenticatedLedgerRows(t *testing.T, p *AuthenticatedLedgerProjection, reference *LedgerProjection) int {
	t.Helper()
	ctx := context.Background()
	view, err := p.store.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer view.Close()
	buckets := append(append([]string{}, projectionBuckets...), projectionListBuckets...)
	want := 0
	if err := reference.db.View(func(tx *bbolt.Tx) error {
		for _, name := range buckets {
			if err := tx.Bucket([]byte(name)).ForEach(func(k, raw []byte) error {
				want++
				lookup, err := authenticatedLedgerKey(name, k)
				if err != nil {
					return err
				}
				actual, found, err := view.Get(ctx, lookup)
				if err != nil || !found || !bytes.Equal(actual, raw) {
					return fmt.Errorf("missing/different %s/%x: found=%v err=%v", name, k, found, err)
				}
				return nil
			}); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	seen, cursor := 0, ""
	for {
		page, err := view.Range(ctx, authenticatedstate.RangeOptions{Cursor: cursor, MaxRows: 64, MaxBytes: 8 << 20, MaxPages: 4096})
		if err != nil {
			t.Fatal(err)
		}
		for _, row := range page.Rows {
			if len(row.Key) < 2 || int(row.Key[0]) > len(buckets) || row.Key[0] == 0 {
				t.Fatal("invalid bucket key", row.Key)
			}
			if err := reference.db.View(func(tx *bbolt.Tx) error {
				value := tx.Bucket([]byte(buckets[int(row.Key[0])-1])).Get(row.Key[1:])
				if value == nil || !bytes.Equal(value, row.Value) {
					return errors.New("extra/different authenticated row")
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			seen++
		}
		if page.Done {
			break
		}
		cursor = page.Cursor
	}
	if seen != want {
		t.Fatal("complete row-set differs", seen, want)
	}
	return seen
}

func authenticatedLedgerNativeFixture(t *testing.T) (string, ChainReport, []Record) {
	t.Helper()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "ledger.wal")
	log := openChained(t, path, nil)
	appendEvent := func(e Event) {
		t.Helper()
		if _, err := log.Append(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	for _, e := range projectionTestAttribution() {
		appendEvent(e)
	}
	alias := projectionTestAttribution()[0]
	alias.EventID = "unit-create-alias"
	appendEvent(alias)
	appendEvent(alias)
	for i := 1; i <= 20; i++ {
		for _, e := range projectionTestLifecycle(i, true) {
			appendEvent(e)
		}
		if i == 10 {
			if _, err := log.Roll(); err != nil {
				t.Fatal(err)
			}
		}
	}
	// Finalization precedes settlement. The original acceptance and lease must
	// survive; late settlement cannot re-finalize or restore active attribution.
	late := projectionTestLifecycle(21, true)
	for _, i := range []int{0, 1, 2, 4, 3, 3} {
		appendEvent(late[i])
	}
	for _, e := range projectionTestLifecycle(22, true)[:3] {
		appendEvent(e)
	}
	// Exercise the maximum domain dependency set with actual native facts.
	for i := 2; i <= domain.MaxRunsPerWorkUnit; i++ {
		e := projectionTestAttribution()[1]
		e.EventID, e.RunID = fmt.Sprintf("run-create-%d", i), fmt.Sprintf("run_%d", i)
		e.Operation = fmt.Sprintf("create-run-%d", i)
		appendEvent(e)
	}
	for _, e := range projectionTestLifecycle(23, false) {
		// Native identities can exceed Bolt's key limit; hash lookup keys must
		// retain complete original identities in authenticated rows and indexes.
		e.ProjectID = strings.Repeat("p", bbolt.MaxKeySize+1)
		e.RequestID = strings.Repeat("r", bbolt.MaxKeySize+1)
		if e.AttemptID != "" {
			e.AttemptID = strings.Repeat("a", bbolt.MaxKeySize+1)
		}
		if e.Kind == EventRequestAccepted {
			e.EventID = strings.Repeat("e", bbolt.MaxKeySize+1)
		}
		appendEvent(e)
	}
	if _, err := log.Compact(1); err != nil {
		t.Fatal(err)
	}
	if _, err := log.Roll(); err != nil {
		t.Fatal(err)
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
	var records []Record
	report, partial, err := InspectReplayAuthenticated(path, testChainKey, func(r Record) error { records = append(records, r); return nil })
	if err != nil || partial {
		t.Fatal(report, partial, err)
	}
	return path, report, records
}

func TestAuthenticatedLedgerNativeSourceCompleteRowsAndRolePrefixes(t *testing.T) {
	path, report, records := authenticatedLedgerNativeFixture(t)
	ctx := context.Background()
	cuts := []*ProjectionPrefix{nil, {}, {Head: Watermark{Generation: 1}}}
	for _, i := range []int{1, 4, 107, 112} {
		r := records[i]
		cuts = append(cuts, &ProjectionPrefix{Head: Watermark{r.Generation, r.Offset, r.Sequence}, Hash: r.Hash})
	}
	for i, prefix := range cuts {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			o := authenticatedLedgerTestOptions(t)
			o.Prefix = prefix
			p, err := BuildAuthenticatedLedgerProjection(ctx, path, testChainKey, filepath.Join(t.TempDir(), "auth"), report, o)
			if err != nil {
				t.Fatal(err)
			}
			defer p.Discard()
			var reference *LedgerProjection
			if prefix == nil {
				reference, err = BuildLedgerProjection(ctx, path, testChainKey, filepath.Join(t.TempDir(), "ref.db"), report)
			} else {
				reference, err = BuildLedgerProjectionPrefix(ctx, path, testChainKey, filepath.Join(t.TempDir(), "ref.db"), report, *prefix)
			}
			if err != nil {
				t.Fatal(err)
			}
			defer reference.Discard()
			rows := compareAuthenticatedLedgerRows(t, p, reference)
			actual, err := p.ReadReplayAnchor(ctx)
			want, refErr := reference.ReadReplayAnchor(ctx)
			if err != nil || refErr != nil || actual != want {
				t.Fatal(actual, want, err, refErr)
			}
			state := NewState()
			for _, r := range records {
				if r.Sequence <= actual.Head.Sequence {
					// Independent balance-only oracle: legacy State retires Run
					// attribution on finalization and cannot replay an attributed
					// late settlement. Remove attribution uniformly for request
					// facts, preserving every native amount, identity and duplicate.
					if r.Event.Kind <= EventRequestFinalized {
						r.Event.WorkUnitID, r.Event.RunID = "", ""
					}
					if err := state.Apply(r); err != nil {
						t.Fatal(err)
					}
				}
			}
			balanceKey := BalanceKey{ProjectID: "prj_1", PeriodID: "2026-10-01", TimezoneVersion: 1}
			balance, err := p.ReadBalance(ctx, balanceKey)
			memory, memoryErr := state.ReadBalance(ctx, balanceKey)
			if err != nil || memoryErr != nil || !reflect.DeepEqual(balance, memory) {
				t.Fatal(balance, memory, err, memoryErr)
			}
			if _, found, err := p.ReadEventPosition(ctx, "invented-event"); found || err != nil {
				t.Fatal("absence proof", found, err)
			}
			t.Logf("native sequence=%d, authenticated rows=%d, catalog buckets=%d", actual.Head.Sequence, rows, len(projectionBuckets)+len(projectionListBuckets))
			if err := p.Discard(); err != nil {
				t.Fatal(err)
			}
			if used := o.Budget.Snapshot().Used; used != (resourcebudget.Cost{}) {
				t.Fatal("discard leaked resource owner", used)
			}
		})
	}
}

func TestAuthenticatedLedgerRejectsInvalidSourceCutsLimitsAndPreservesForeignOutput(t *testing.T) {
	path, report, records := prefixBuildFixture(t)
	r := records[1]
	valid := ProjectionPrefix{Head: Watermark{r.Generation, r.Offset, r.Sequence}, Hash: r.Hash}
	for _, mode := range []string{"wrong-report", "wrong-hash", "frame-interior", "foreign-output", "append-limit", "cancel", "pressure"} {
		t.Run(mode, func(t *testing.T) {
			o := authenticatedLedgerTestOptions(t)
			prefix, expected := valid, report
			o.Prefix = &prefix
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			output := filepath.Join(t.TempDir(), "auth")
			switch mode {
			case "wrong-report":
				expected.ChainHash[0] ^= 1
			case "wrong-hash":
				prefix.Hash[0] ^= 1
			case "frame-interior":
				prefix.Head.Offset--
			case "foreign-output":
				if err := os.Mkdir(output, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(output, "foreign"), []byte("preserve"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "append-limit":
				o.MaxAppendBytes = 64
			case "cancel":
				cancel()
			case "pressure":
				if err := o.Budget.SetPressure(resourcebudget.Pressured); err != nil {
					t.Fatal(err)
				}
			}
			p, err := BuildAuthenticatedLedgerProjection(ctx, path, testChainKey, output, expected, o)
			if p != nil || err == nil {
				t.Fatal("invalid candidate escaped", p, err)
			}
			if mode == "pressure" && !errors.Is(err, resourcebudget.ErrPressure) {
				t.Fatal("source opened despite pressure", err)
			}
			if mode == "foreign-output" {
				data, err := os.ReadFile(filepath.Join(output, "foreign"))
				if err != nil || string(data) != "preserve" {
					t.Fatal(data, err)
				}
			} else if _, err := os.Stat(output); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("failed build retained private candidate", err)
			}
			if used := o.Budget.Snapshot().Used; used != (resourcebudget.Cost{}) {
				t.Fatal("failed build leaked resource owner", used)
			}
		})
	}
}

func TestAuthenticatedLedgerPrefixStillAuthenticatesExcludedSuffix(t *testing.T) {
	path, report, records := prefixBuildFixture(t)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, writeErr := f.Write([]byte{5, 0, 0}) // Incomplete native frame in excluded suffix.
	if err := errors.Join(writeErr, f.Close()); err != nil {
		t.Fatal(err)
	}
	r := records[1]
	o := authenticatedLedgerTestOptions(t)
	o.Prefix = &ProjectionPrefix{Head: Watermark{r.Generation, r.Offset, r.Sequence}, Hash: r.Hash}
	output := filepath.Join(t.TempDir(), "auth")
	p, err := BuildAuthenticatedLedgerProjection(context.Background(), path, testChainKey, output, report, o)
	if p != nil || !errors.Is(err, ErrProjectionSource) {
		t.Fatal(p, err)
	}
	if _, err := os.Stat(output); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("incomplete native suffix candidate remained", err)
	}
}

func TestAuthenticatedLedgerReadFailureAndCloseNeverReturnBusinessAbsence(t *testing.T) {
	path, report, records := prefixBuildFixture(t)
	r := records[1]
	for _, mode := range []string{"corrupt-pack", "close"} {
		t.Run(mode, func(t *testing.T) {
			o := authenticatedLedgerTestOptions(t)
			o.Prefix = &ProjectionPrefix{Head: Watermark{r.Generation, r.Offset, r.Sequence}, Hash: r.Hash}
			output := filepath.Join(t.TempDir(), "auth")
			p, err := BuildAuthenticatedLedgerProjection(context.Background(), path, testChainKey, output, report, o)
			if err != nil {
				t.Fatal(err)
			}
			defer p.Discard()
			wantErr := authenticatedstate.ErrCorrupt
			if mode == "close" {
				wantErr = ErrProjectionUnavailable
				if err := p.Close(); err != nil {
					t.Fatal(err)
				}
				// Creator cleanup/control stays owned with the persistent namespace;
				// Close retires IO, and Discard later retires this fixed receipt.
				if used := o.Budget.Snapshot().Used; used.RAM != 4096+resourcebudget.ClaimRAMBytes || used.Disk == 0 {
					t.Fatal("Close refunded persistent disk or retained resident RAM", used)
				}
			} else {
				info, err := os.Stat(filepath.Join(output, "pages"))
				if err != nil {
					t.Fatal(err)
				}
				if err := os.Truncate(filepath.Join(output, "pages"), info.Size()-1); err != nil {
					t.Fatal(err)
				}
			}
			for _, id := range []string{r.Event.EventID, "invented-event"} {
				value, found, err := p.ReadEventPosition(context.Background(), id)
				if !errors.Is(err, wantErr) || found || value != (Watermark{}) {
					t.Fatal("failed owner returned business absence", value, found, err)
				}
			}
			if err := p.Discard(); err != nil {
				t.Fatal(err)
			}
			if used := o.Budget.Snapshot().Used; used != (resourcebudget.Cost{}) {
				t.Fatal("final captured discard leaked", used)
			}
		})
	}
}
