package authenticatedstate

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestSelectedHistoricalRootPreservesAheadPagesAndAppendsNewBranch(t *testing.T) {
	for _, count := range []int{0, 8} {
		t.Run(map[int]string{0: "empty", 8: "nonempty"}[count], func(t *testing.T) {
			ctx := context.Background()
			owner, o := testStore(t, count)
			schema := SealRootSchema{Kind: SealRootState, Component: SealComponentLedger, IndexID: 1, Codec: 1}
			saved, err := owner.SealRootReference(ctx, schema)
			if err != nil {
				t.Fatal(err)
			}
			savedID, err := owner.RootIdentity(ctx)
			if err != nil {
				t.Fatal(err)
			}
			savedRows := storeRows(t, owner)
			if _, err := owner.Update(ctx, nextCut(1), func(tx *Transaction) error { return tx.Put([]byte("ahead"), []byte("unsealed")) }); err != nil {
				t.Fatal(err)
			}
			ahead, err := owner.SealRootReference(ctx, schema)
			if err != nil {
				t.Fatal(err)
			}
			aheadBytes, err := os.ReadFile(filepath.Join(owner.dir, "pages"))
			if err != nil {
				t.Fatal(err)
			}
			if err := owner.Close(); err != nil {
				t.Fatal(err)
			}
			o.ExpectedSealRoot, o.ExpectedSealSchema, o.ExpectedRoot = &saved, &schema, &savedID
			selected, err := OpenStore(ctx, owner.dir, o)
			if err != nil {
				t.Fatal(err)
			}
			if rows := storeRows(t, selected); !reflect.DeepEqual(rows, savedRows) {
				t.Fatal("selected future values", rows, savedRows)
			}
			if current, err := selected.SealRootReference(ctx, schema); err != nil || current != saved {
				t.Fatal(current, err)
			}
			now, err := os.ReadFile(filepath.Join(owner.dir, "pages"))
			if err != nil || !bytes.Equal(now, aheadBytes) {
				t.Fatal("historical selection truncated ahead pages", err)
			}
			receipt, err := selected.Update(ctx, nextCut(2), func(tx *Transaction) error { return tx.Put([]byte("branch"), []byte("permitted")) })
			if err != nil {
				t.Fatal(err)
			}
			if receipt.Revision != ahead.State.Revision+1 {
				t.Fatal("reused physical revision", receipt)
			}
			branch, err := selected.SealRootReference(ctx, schema)
			if err != nil {
				t.Fatal(err)
			}
			if branch.State.Extent <= ahead.State.Extent {
				t.Fatal("did not append after physical extent")
			}
			now, err = os.ReadFile(filepath.Join(owner.dir, "pages"))
			if err != nil || !bytes.Equal(now[:len(aheadBytes)], aheadBytes) {
				t.Fatal("overwrote ahead pages", err)
			}
			if err := selected.Close(); err != nil {
				t.Fatal(err)
			}
			// The independently selected ahead commitment also remains readable, even
			// after branching from the last globally sealed cut.
			o.ExpectedRoot = nil
			o.ExpectedCut = ahead.State.Cut
			o.ExpectedSealRoot = &ahead
			reopened, err := OpenStore(ctx, owner.dir, o)
			if err != nil {
				t.Fatal(err)
			}
			rows := storeRows(t, reopened)
			found := false
			for _, row := range rows {
				if string(row.Key) == "ahead" {
					found = true
				}
				if string(row.Key) == "branch" {
					t.Fatal("historical root leaked newer branch")
				}
			}
			if !found {
				t.Fatal("ahead root lost")
			}
			if err := reopened.Close(); err != nil {
				t.Fatal(err)
			}
			if err := owner.AdoptClosedSuccessor(reopened); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestHistoricalRootCommitmentAndPageRefuseBeforeCleanup(t *testing.T) {
	ctx := context.Background()
	owner, o := testStore(t, 8)
	schema := SealRootSchema{Kind: SealRootState, Component: SealComponentLedger, IndexID: 1, Codec: 1}
	saved, err := owner.SealRootReference(ctx, schema)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := owner.Update(ctx, nextCut(1), func(tx *Transaction) error { return tx.Put([]byte("ahead"), []byte("value")) }); err != nil {
		t.Fatal(err)
	}
	if err := owner.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(owner.dir, "pages")
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	orphan := append(bytes.Clone(original), []byte("unpublished orphan")...)
	if err := os.WriteFile(path, orphan, 0600); err != nil {
		t.Fatal(err)
	}
	o.ExpectedSealSchema = &schema
	before := o.Budget.Snapshot().Used
	mutations := []func(*SealRoot){
		func(r *SealRoot) { r.State.Scope[0] ^= 1 }, func(r *SealRoot) { r.State.Cut[0] ^= 1 }, func(r *SealRoot) { r.State.Digest[0] ^= 1 },
		func(r *SealRoot) { r.State.PackID[0] ^= 1 }, func(r *SealRoot) { r.State.Revision++ }, func(r *SealRoot) { r.State.Extent++ }, func(r *SealRoot) { r.State.Height++ },
	}
	for i, mutate := range mutations {
		wrong := saved
		mutate(&wrong)
		o.ExpectedSealRoot = &wrong
		if next, err := OpenStore(ctx, owner.dir, o); next != nil || !errors.Is(err, ErrRecovery) {
			t.Fatalf("field %d: %v %v", i, next, err)
		}
		got, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(got, orphan) || o.Budget.Snapshot().Used != before {
			t.Fatal("refusal mutated cleanup or credit", i, err)
		}
	}
	corrupt := bytes.Clone(orphan)
	corrupt[saved.Offset] ^= 1
	if err := os.WriteFile(path, corrupt, 0600); err != nil {
		t.Fatal(err)
	}
	o.ExpectedSealRoot = &saved
	if next, err := OpenStore(ctx, owner.dir, o); next != nil || !errors.Is(err, ErrRecovery) {
		t.Fatal("selected page tamper accepted", next, err)
	}
	got, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(got, corrupt) {
		t.Fatal("page rejection cleaned orphan", err)
	}
	if err := os.WriteFile(path, original, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestCandidateSealV2CarriesCompleteStateAndRejectsKindDowngrade(t *testing.T) {
	ctx := context.Background()
	owner, _ := testStore(t, 8)
	schema := SealRootSchema{Kind: SealRootState, Component: SealComponentLedger, IndexID: 1, Codec: 1}
	root, err := owner.SealRootReference(ctx, schema)
	if err != nil {
		t.Fatal(err)
	}
	s := sealCandidateFixture()
	s.Roots[0] = root
	hard := sealHardFor(s)
	key := bytes.Repeat([]byte{7}, 32)
	raw, err := EncodeSealCandidate(s, key, hard)
	if err != nil {
		t.Fatal(err)
	}
	got, err := DecodeSealCandidate(raw, key, hard)
	if err != nil || got != s {
		t.Fatal(got, err)
	}
	s.Roots[0].Kind = SealRootNative
	s.Roots[0].State = StateRootCommitment{}
	if _, err := EncodeSealCandidate(s, key, hard); err == nil {
		t.Fatal("independent inventory allowed removal of full commitment")
	}
	// Component commitments cannot exceed the envelope's claimed reader limits.
	s.Roots[0] = root
	s.MaxTreeHeight = 1
	if _, err := EncodeSealCandidate(s, key, hard); err == nil {
		t.Fatal("state height exceeded advertised limit")
	}
	s.MaxTreeHeight = 32
	s.MaxPageBytes = 1
	if _, err := EncodeSealCandidate(s, key, hard); err == nil {
		t.Fatal("page exceeded advertised limit")
	}
	// Every bounded root fits within the unchanged 16 KiB wire ceiling.
	s = sealCandidateFixture()
	s.RootCount = maxSealRoots
	for i := 0; i < maxSealRoots; i++ {
		s.Roots[i] = root
		s.Roots[i].IndexID = uint16(i + 1)
	}
	raw, err = EncodeSealCandidate(s, key, sealHardFor(s))
	if err != nil || len(raw) > candidateSealMaxBytes {
		t.Fatal(len(raw), err)
	}
}
