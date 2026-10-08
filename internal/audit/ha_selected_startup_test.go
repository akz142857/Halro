package audit

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/authenticatedstate"
)

func TestHASelectedNativeActualModesSuffixAndRefusals(t *testing.T) {
	for _, replica := range []bool{false, true} {
		t.Run(map[bool]string{false: "primary", true: "replica"}[replica], func(t *testing.T) {
			ctx := context.Background()
			f := newStartupCacheFixture(t, 1)
			if _, err := f.native.Append(ctx, validEvent(2, "key.create")); err != nil {
				t.Fatal(err)
			}
			full := f.native.Summary()
			if err := f.native.Close(); err != nil {
				t.Fatal(err)
			}
			seal, found, err := f.selected.Publisher.Current()
			if err != nil || !found {
				t.Fatal(err)
			}
			seal.Generation++
			seal.Role = authenticatedstate.SealPrimary
			if replica {
				seal.Role = authenticatedstate.SealReplica
			}
			seal.Cluster = [32]byte{11}
			seal.Member = [32]byte{12}
			seal.Term = 1
			seal.OrderingTerm = 1
			seal.OrderingIndex = 9
			seal.OrderingOffset = 256
			seal.OrderingMAC = [32]byte{13}
			if _, err := f.selected.Publisher.Publish(ctx, seal); err != nil {
				t.Fatal(err)
			}
			a := f.selected.Authority
			a.Role = seal.Role
			a.Cluster = seal.Cluster
			a.Member = seal.Member
			a.Term = seal.Term
			a.OrderingIndex = seal.OrderingIndex
			a.OrderingOffset = seal.OrderingOffset
			a.OrderingMAC = seal.OrderingMAC
			callback := func(ctx context.Context, visit func(context.Context, authenticatedstate.SealEligibility, HASelectedNativePosition) error) error {
				return visit(ctx, a, HASelectedNativePosition{Sequence: full.Records, Hash: full.LastHash})
			}
			o := HASelectedSealOpenOptions{SelectedSealOpenOptions: f.selected, WithAuthority: callback}
			options := f.options
			options.Replica = replica
			if !replica {
				options.BeforeDurable = func(DurableBatch) (AppendAdmission, error) { return nil, nil }
				options.AfterDurable = func(DurableBatch) error { return nil }
			}
			if n, _, err := OpenWithSelectedSeal(ctx, f.path, f.key, options, f.selected); err == nil || n != nil {
				t.Fatal("old Standalone constructor accepted HA", n, err)
			}
			missing := o
			missing.WithAuthority = nil
			if n, _, err := OpenWithHASelectedSeal(ctx, f.path, f.key, options, missing); err == nil || n != nil {
				t.Fatal("missing independent role permission accepted", n, err)
			}
			nonvisit := o
			nonvisit.WithAuthority = func(context.Context, func(context.Context, authenticatedstate.SealEligibility, HASelectedNativePosition) error) error {
				return nil
			}
			if n, _, err := OpenWithHASelectedSeal(ctx, f.path, f.key, options, nonvisit); err == nil || n != nil {
				t.Fatal("empty permission callback accepted", n, err)
			}
			foreign := o
			foreign.WithAuthority = func(ctx context.Context, v func(context.Context, authenticatedstate.SealEligibility, HASelectedNativePosition) error) error {
				b := a
				b.Member = [32]byte{99}
				return v(ctx, b, HASelectedNativePosition{Sequence: full.Records, Hash: full.LastHash})
			}
			if n, _, err := OpenWithHASelectedSeal(ctx, f.path, f.key, options, foreign); err == nil || n != nil {
				t.Fatal("foreign member accepted", n, err)
			}
			incorrect := o
			incorrect.WithAuthority = func(ctx context.Context, v func(context.Context, authenticatedstate.SealEligibility, HASelectedNativePosition) error) error {
				return v(ctx, a, HASelectedNativePosition{Sequence: 1, Hash: f.first.Hash})
			}
			prefixNative, prefixReceipt, prefixErr := OpenWithHASelectedSeal(ctx, f.path, f.key, options, incorrect)
			if replica {
				if prefixErr != nil || prefixNative == nil || prefixReceipt.Head != full || prefixReceipt.Allowed != prefixReceipt.Saved || prefixReceipt.Allowed.Records != 1 {
					t.Fatal("actual received tail became the applied prefix", prefixReceipt, prefixErr)
				}
				if err := prefixNative.Close(); err != nil {
					t.Fatal(err)
				}
			} else if prefixErr == nil || prefixNative != nil {
				t.Fatal("Primary omitted actual durable suffix", prefixNative, prefixErr)
			}
			for _, permission := range []HASelectedNativePosition{{Sequence: 1, Hash: [32]byte{99}}, {Sequence: 0}} {
				wrong := o
				wrong.WithAuthority = func(ctx context.Context, v func(context.Context, authenticatedstate.SealEligibility, HASelectedNativePosition) error) error {
					return v(ctx, a, permission)
				}
				if n, _, err := OpenWithHASelectedSeal(ctx, f.path, f.key, options, wrong); err == nil || n != nil {
					t.Fatal("foreign hash or below-saved prefix accepted", permission, n, err)
				}
			}
			n, receipt, err := OpenWithHASelectedSeal(ctx, f.path, f.key, options, o)
			if err != nil {
				t.Fatal(err)
			}
			f.native = n
			if receipt.Saved.Records != 1 || receipt.Head != full || receipt.Allowed != full || receipt.DecodedRecords != 1 || !receipt.HistoricalAuditPending || n.SelectedStartupReady() {
				t.Fatal("selected suffix receipt", receipt)
			}
			if _, err := n.Append(ctx, validEvent(2, "key.create")); err == nil {
				t.Fatal("pending history allowed semantic append")
			}
			audit, err := n.StartSelectedStartupAudit(ctx, options.CacheBudget, StartupAuditOptions{Deadline: time.Second, ProgressTimeout: time.Second})
			if err != nil {
				t.Fatal(err)
			}
			if err := audit.Wait(ctx); err != nil {
				t.Fatal(err)
			}
			if !n.SelectedStartupReady() {
				t.Fatal("joined full native audit did not grant readiness")
			}
			record, err := n.Append(ctx, validEvent(2, "key.create"))
			if replica && err == nil || !replica && (err != nil || record.Sequence != 2) {
				t.Fatal("actual native role/dedup", record, err)
			}
			if err := n.Close(); err != nil && !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
		})
	}
}
