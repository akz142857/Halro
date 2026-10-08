package ledger

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	bbolt "go.etcd.io/bbolt"
)

func TestProjectionBalanceCorruptionCannotReadOrAdvance(t *testing.T) {
	for _, mutation := range []string{"reserved", "committed", "input", "output", "unknown", "missing", "null"} {
		for _, api := range []string{"ordinary-read", "authenticated-read", "ordinary-apply", "authenticated-advance"} {
			t.Run(mutation+"/"+api, func(t *testing.T) {
				p, native, ref, directory := recoveryViewFixture(t)
				ctx := context.Background()
				event := projectionTestLifecycle(4, true)[0]
				key := BalanceKey{event.ProjectID, event.PeriodID, event.PeriodTimezoneVersion}
				balance, err := ref.ReadBalance(ctx, key)
				if err != nil {
					t.Fatal(err)
				}
				var raw []byte
				switch mutation {
				case "reserved":
					balance.ReservedMicrosUSD = -1
				case "committed":
					balance.CommittedMicrosUSD = -1
				case "input":
					balance.InputTokens = -1
				case "output":
					balance.OutputTokens = -1
				case "unknown":
					balance.UnknownAttempts = -1
				case "missing":
					raw = []byte(`{}`)
				case "null":
					raw = []byte(`{"ReservedMicrosUSD":null,"CommittedMicrosUSD":0,"InputTokens":0,"OutputTokens":0,"UnknownAttempts":0}`)
				}
				if raw == nil {
					raw, err = json.Marshal(balance)
					if err != nil {
						t.Fatal(err)
					}
				}
				auth := api == "authenticated-read" || api == "authenticated-advance"
				if auth {
					mutatePoint(t, p, "balances", projectionBalanceKey(key), raw)
				} else if err := ref.db.Update(func(tx *bbolt.Tx) error { return tx.Bucket([]byte("balances")).Put(projectionBalanceKey(key), raw) }); err != nil {
					t.Fatal(err)
				}
				beforeFees := p.budget.Snapshot()
				if api == "ordinary-read" || api == "authenticated-read" {
					var got Balance
					if auth {
						got, err = p.ReadBalance(ctx, key)
					} else {
						got, err = ref.ReadBalance(ctx, key)
					}
					if got != (Balance{}) {
						t.Error("corrupt balance escaped", got)
					}
				} else {
					before, e := ref.ReadReplayAnchor(ctx)
					if e != nil {
						t.Fatal(e)
					}
					root, e := os.ReadFile(filepath.Join(directory, "active.root"))
					if e != nil {
						t.Fatal(e)
					}
					// A new reservation would turn -1 into a plausible positive value.
					event = projectionTestLifecycle(4, true)[1]
					event.AttemptID = "att_new_balance"
					event.EventID = "evt_new_balance"
					record, _, e := native.AppendRecordWithReceipt(ctx, event)
					if e != nil {
						t.Fatal(e)
					}
					if auth {
						_, err = p.AdvanceOneFromNative(ctx, native, ProjectionPrefix{Head: Watermark{record.Generation, record.Offset, record.Sequence}, Hash: record.Hash})
						if after, e := os.ReadFile(filepath.Join(directory, "active.root")); e != nil || !bytes.Equal(root, after) {
							t.Error("failed balance validation published a root", e)
						}
					} else {
						err = ref.Apply(ctx, record)
						if after, e := ref.ReadReplayAnchor(ctx); e != nil || after != before {
							t.Error("corrupt balance advanced cut", after, e)
						}
						if _, found, e := ref.ReadLease(ctx, event.AttemptID); found || e != nil {
							t.Error("failed balance apply published a lease", found, e)
						}
					}
					if head, hash, _ := native.ChainHead(); head.Sequence != record.Sequence || hash != record.Hash {
						t.Error("durable native fact lost")
					}
				}
				if !errors.Is(err, ErrProjectionCorrupt) {
					t.Fatal("bad balance accepted", err)
				}
				if auth && api == "authenticated-read" {
					if _, found, e := p.ReadLease(ctx, "absent"); found || !errors.Is(e, ErrProjectionCorrupt) {
						t.Error("corrupt owner fabricated absence", found, e)
					}
				}
				if after := p.budget.Snapshot(); after.Used != beforeFees.Used || after.NormalClaims != beforeFees.NormalClaims {
					t.Error("lost persistent fees or retained scratch", beforeFees, after)
				}
			})
		}
	}
}

func TestProjectionGovernanceAliasCorruptionCannotAdvance(t *testing.T) {
	for _, mutation := range []string{"project", "operation", "key", "fingerprint", "work-unit", "run"} {
		for _, auth := range []bool{false, true} {
			t.Run(mutation+map[bool]string{false: "/ordinary", true: "/authenticated"}[auth], func(t *testing.T) {
				p, native, ref, directory := recoveryViewFixture(t)
				ctx := context.Background()
				event := projectionTestAttribution()[0]
				row, found, err := ref.ReadGovernanceIdempotency(ctx, event.ProjectID, event.Operation, event.IdempotencyKeyHash)
				if err != nil || !found {
					t.Fatal(found, err)
				}
				switch mutation {
				case "project":
					row.ProjectID = "different"
				case "operation":
					row.Operation += "different"
				case "key":
					row.KeyHash = "wrong"
				case "fingerprint":
					row.RequestFingerprint = "wrong"
				case "work-unit":
					row.WorkUnitID = "wrong"
				case "run":
					row.RunID = "wrong"
				}
				key := projectionTuple(event.ProjectID, event.Operation, event.IdempotencyKeyHash)
				if auth {
					mutatePoint(t, p, "idempotency", key, row)
				} else if err := ref.db.Update(func(tx *bbolt.Tx) error { return projectionPut(tx, "idempotency", key, row) }); err != nil {
					t.Fatal(err)
				}
				event = projectionTestAttribution()[0]
				event.EventID = "evt_governance_alias_index"
				before, e := ref.ReadReplayAnchor(ctx)
				if e != nil {
					t.Fatal(e)
				}
				root, e := os.ReadFile(filepath.Join(directory, "active.root"))
				if e != nil {
					t.Fatal(e)
				}
				record, _, e := native.AppendRecordWithReceipt(ctx, event)
				if e != nil {
					t.Fatal(e)
				}
				if auth {
					_, err = p.AdvanceOneFromNative(ctx, native, ProjectionPrefix{Head: Watermark{record.Generation, record.Offset, record.Sequence}, Hash: record.Hash})
					if after, e := os.ReadFile(filepath.Join(directory, "active.root")); e != nil || !bytes.Equal(after, root) {
						t.Error("bad alias advanced authenticated root", e)
					}
				} else {
					err = ref.Apply(ctx, record)
					if after, e := ref.ReadReplayAnchor(ctx); e != nil || after != before {
						t.Error("bad alias advanced ordinary cut", after, e)
					}
				}
				if !errors.Is(err, ErrProjectionCorrupt) {
					t.Fatal("invalid alias hydration accepted", err)
				}
				if head, hash, _ := native.ChainHead(); head.Sequence != record.Sequence || hash != record.Hash {
					t.Error("lost durable governance alias fact")
				}
			})
		}
	}
}

func TestProjectionEventFirstPositionRejectsInvalidCutWithoutResult(t *testing.T) {
	for _, mutation := range []string{"generation", "offset", "sequence", "half-index", "digest-short", "digest-long"} {
		for _, auth := range []bool{false, true} {
			t.Run(mutation+map[bool]string{false: "/ordinary", true: "/authenticated"}[auth], func(t *testing.T) {
				p, _, ref, _ := recoveryViewFixture(t)
				ctx := context.Background()
				event := projectionTestAttribution()[0]
				anchor, err := ref.ReadReplayAnchor(ctx)
				if err != nil {
					t.Fatal(err)
				}
				first := anchor.Head
				switch mutation {
				case "generation":
					first.Generation++
				case "offset":
					first.Offset++
				case "sequence":
					first.Sequence++
				}
				bucket, value := "event_positions", any(first)
				if mutation == "half-index" {
					bucket, value = "events", nil
				}
				if mutation == "digest-short" || mutation == "digest-long" {
					digest, e := eventDigest(event)
					if e != nil {
						t.Fatal(e)
					}
					items := make([]int, 32)
					for i, b := range digest {
						items[i] = int(b)
					}
					if mutation == "digest-short" {
						items = items[:31]
					} else {
						items = append(items, 0)
					}
					bucket, value = "events", items
				}
				if auth {
					mutatePoint(t, p, bucket, projectionIDKey(event.EventID), value)
				} else if err := ref.db.Update(func(tx *bbolt.Tx) error {
					if value == nil {
						return tx.Bucket([]byte(bucket)).Delete(projectionIDKey(event.EventID))
					}
					return projectionPut(tx, bucket, projectionIDKey(event.EventID), value)
				}); err != nil {
					t.Fatal(err)
				}
				var found bool
				if auth {
					first, found, err = p.ReadEventPosition(ctx, event.EventID)
				} else {
					first, found, err = ref.ReadEventPosition(ctx, event.EventID)
				}
				if !errors.Is(err, ErrProjectionCorrupt) || found || first != (Watermark{}) {
					t.Fatal("invalid idempotency position escaped", first, found, err)
				}
			})
		}
	}
}

func TestProjectionDuplicateCannotAdoptFutureFirstOccurrence(t *testing.T) {
	for _, mutation := range []string{"generation", "offset", "sequence"} {
		for _, auth := range []bool{false, true} {
			t.Run(mutation+map[bool]string{false: "/ordinary", true: "/authenticated"}[auth], func(t *testing.T) {
				p, native, ref, directory := recoveryViewFixture(t)
				ctx := context.Background()
				event := projectionTestAttribution()[0]
				before, err := ref.ReadReplayAnchor(ctx)
				if err != nil {
					t.Fatal(err)
				}
				first := before.Head
				switch mutation {
				case "generation":
					first.Generation++
				case "offset":
					first.Offset++
				case "sequence":
					first.Sequence++
				}
				if auth {
					mutatePoint(t, p, "event_positions", projectionIDKey(event.EventID), first)
				} else if err := ref.db.Update(func(tx *bbolt.Tx) error {
					return projectionPut(tx, "event_positions", projectionIDKey(event.EventID), first)
				}); err != nil {
					t.Fatal(err)
				}
				root, err := os.ReadFile(filepath.Join(directory, "active.root"))
				if err != nil {
					t.Fatal(err)
				}
				// A physical duplicate cannot make a future first position valid.
				record, _, err := native.AppendRecordWithReceipt(ctx, event)
				if err != nil {
					t.Fatal(err)
				}
				if auth {
					_, err = p.AdvanceOneFromNative(ctx, native, ProjectionPrefix{Head: Watermark{record.Generation, record.Offset, record.Sequence}, Hash: record.Hash})
					if after, e := os.ReadFile(filepath.Join(directory, "active.root")); e != nil || !bytes.Equal(root, after) {
						t.Error("bad first occurrence published a root", e)
					}
				} else {
					err = ref.Apply(ctx, record)
					if after, e := ref.ReadReplayAnchor(ctx); e != nil || after != before {
						t.Error("bad first occurrence advanced cut", after, e)
					}
				}
				if !errors.Is(err, ErrProjectionCorrupt) {
					t.Fatal("future first occurrence adopted through duplicate", err)
				}
				if head, hash, _ := native.ChainHead(); head.Sequence != record.Sequence || hash != record.Hash {
					t.Error("durable duplicate discarded")
				}
			})
		}
	}
}
