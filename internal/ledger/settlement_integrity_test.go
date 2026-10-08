package ledger

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	bbolt "go.etcd.io/bbolt"
)

func TestProjectionSettlementCorruptionCannotEscapeThroughPointOrPages(t *testing.T) {
	for _, mutation := range []string{"digest", "cost", "cost-known", "kind", "position-zero", "position-generation", "position-offset", "position-sequence"} {
		for _, api := range []string{"point", "settled-page", "request-page", "pricing-view", "recovery-view"} {
			if api == "point" && strings.HasPrefix(mutation, "position-") {
				continue // This test's point contract is canonical settlement content.
			}
			t.Run(mutation+"/"+api, func(t *testing.T) {
				p, native, ref, _ := recoveryViewFixture(t)
				ctx := context.Background()
				const attempt = "att_000002"
				row, found, err := ref.ReadSettlement(ctx, attempt)
				if err != nil || !found {
					t.Fatal(found, err)
				}
				anchor, err := ref.ReadReplayAnchor(ctx)
				if err != nil {
					t.Fatal(err)
				}
				bucket, key, value := "settled", projectionIDKey(attempt), any(row)
				switch mutation {
				case "digest":
					row.SettlementDigest = "sha256:" + strings.Repeat("0", 64)
				case "cost":
					row.CostMicrosUSD++
				case "cost-known":
					row.CostKnown = !row.CostKnown
				case "kind":
					row.Settlement.Kind = EventRequestFinalized
				default:
					position := Watermark{Generation: anchor.Head.Generation, Sequence: anchor.Head.Sequence, Offset: anchor.Head.Offset}
					switch mutation {
					case "position-zero":
						position = Watermark{}
					case "position-generation":
						position.Generation++
					case "position-offset":
						position.Offset++
					case "position-sequence":
						position.Sequence++
					}
					bucket, key, value = "meta", projectionTuple("settlement", attempt), position
				}
				if bucket == "settled" {
					value = row
				}
				if api == "pricing-view" || api == "recovery-view" {
					mutatePoint(t, p, bucket, key, value)
				} else if err := ref.db.Update(func(tx *bbolt.Tx) error { return projectionPut(tx, bucket, key, value) }); err != nil {
					t.Fatal(err)
				}
				before := p.budget.Snapshot()
				head, hash, _ := native.ChainHead()
				limits := ProjectionPageLimits{Rows: 64, Bytes: 8 << 20}
				switch api {
				case "point":
					v, found, e := ref.ReadSettlement(ctx, attempt)
					if !errors.Is(e, ErrProjectionCorrupt) || found || !reflect.DeepEqual(v, SettledAttempt{}) {
						t.Fatal("corrupt point result escaped", v, found, e)
					}
				case "settled-page":
					v, e := ref.SettlementPage(ctx, ProjectionCursor{}, limits)
					if !errors.Is(e, ErrProjectionCorrupt) || !reflect.DeepEqual(v, ProjectionPage[SettledAttempt]{}) {
						t.Fatal("corrupt page or progress escaped", v, e)
					}
				case "request-page":
					v, e := ref.RequestAttemptPage(ctx, row.Settlement.RequestID, ProjectionCursor{}, limits)
					if !errors.Is(e, ErrProjectionCorrupt) || !reflect.DeepEqual(v, ProjectionPage[ProjectedAttempt]{}) {
						t.Fatal("corrupt request attempt escaped", v, e)
					}
				case "pricing-view":
					err = p.WithFreshPricingView(ctx, func(v AuthenticatedPricingView) error {
						page, e := v.SettlementPage(ctx, ProjectionCursor{}, limits)
						if !errors.Is(e, ErrProjectionCorrupt) || !reflect.DeepEqual(page, ProjectionPage[SettledAttempt]{}) {
							t.Error("corrupt pricing page escaped", page, e)
						}
						return nil // An ignored read failure cannot accept the pin.
					})
				case "recovery-view":
					err = p.WithRecoveryView(ctx, func(v AuthenticatedRecoveryView) error {
						page, e := v.RequestAttemptPage(ctx, "req_000002", "", limits)
						if !errors.Is(e, ErrProjectionCorrupt) || !reflect.DeepEqual(page, AuthenticatedRecoveryPage[ProjectedAttempt]{}) {
							t.Error("corrupt recovery page escaped", page, e)
						}
						return nil
					})
				}
				if api == "pricing-view" || api == "recovery-view" {
					if !errors.Is(err, ErrProjectionCorrupt) {
						t.Fatal("ignored corrupt page accepted callback", err)
					}
					if _, _, e := p.ReadSettlement(ctx, "absent"); e == nil {
						t.Fatal("corrupt owner fabricated absence")
					}
				}
				if after := p.budget.Snapshot(); after.Used != before.Used || after.Owners != before.Owners || after.NormalClaims != before.NormalClaims {
					t.Fatal("failed reader lost persistent fees or retained IO", before, after)
				}
				if actual, actualHash, _ := native.ChainHead(); actual != head || actualHash != hash || native.Status().Load() != AccountingHealthy {
					t.Fatal("derived read mutated original native facts", actual)
				}
			})
		}
	}
}

func TestProjectionSettlementPageDropsValidPrefixBeforeCorruptRow(t *testing.T) {
	p := projectionTestStore(t)
	ctx := context.Background()
	sequence := projectionApplyEvents(t, p, projectionTestLifecycle(1, false), 0)
	projectionApplyEvents(t, p, projectionTestLifecycle(2, false), sequence)
	if err := p.db.Update(func(tx *bbolt.Tx) error {
		row, _, err := projectionGet[SettledAttempt](tx, "settled", projectionIDKey("att_000002"))
		if err != nil {
			return err
		}
		row.CostMicrosUSD++
		raw, err := json.Marshal(row)
		if err != nil {
			return err
		}
		return tx.Bucket([]byte("settled")).Put(projectionIDKey("att_000002"), raw)
	}); err != nil {
		t.Fatal(err)
	}
	page, err := p.SettlementPage(ctx, ProjectionCursor{}, ProjectionPageLimits{Rows: 64, Bytes: 8 << 20})
	if !errors.Is(err, ErrProjectionCorrupt) || !reflect.DeepEqual(page, ProjectionPage[SettledAttempt]{}) {
		t.Fatal("valid prefix leaked from failed atomic page", page, err)
	}
}
