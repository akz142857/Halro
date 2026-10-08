package authroot

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"math"
	"path/filepath"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/vault"
)

func sealFixture() (stateSeal, []byte, sealReaderContext) {
	s := stateSeal{Identity: sealIdentity{Instance: "test-instance"}, Role: sealStandalone, Coverage: sealAuthenticated,
		Generation: 1, Revision: 1, MetadataSchema: 45, MetadataBaselineID: sha256.Sum256([]byte("accepted-baseline-id")), MetadataBaselineRoot: sha256.Sum256([]byte("accepted-baseline-root"))}
	for i := range s.Native {
		s.Native[i].HeaderDigest = sha256.Sum256([]byte{byte(i + 1)})
		if i < 2 {
			s.Native[i].Generation, s.Native[i].FileRevision = 1, 1
		}
	}
	for i := range s.Roots {
		s.Roots[i] = root{scope: uint16(i + 1), generation: 1, revision: 1, ref: emptyRef(uint16(i + 1))}
	}
	return s, bytes.Repeat([]byte{0x38}, 32), sealReaderContext{s.Identity, s.MetadataSchema}
}

func TestSealAuthenticatesEveryEncodedByteAndReaderContext(t *testing.T) {
	s, key, expected := sealFixture()
	b, err := signSeal(s, key)
	if err != nil || len(b) != sealBytes || sealBytes > sealLimit {
		t.Fatal(len(b), err)
	}
	actual, err := verifySeal(b, key, expected)
	if err != nil || actual != s {
		t.Fatal(actual, err)
	}
	for i := range b {
		bad := bytes.Clone(b)
		bad[i] ^= 1
		if got, err := verifySeal(bad, key, expected); err == nil || got != (stateSeal{}) {
			t.Fatal("changed byte accepted/partial result", i, err)
		}
	}
	for n := 0; n < len(b); n++ {
		if _, err := verifySeal(b[:n], key, expected); err == nil {
			t.Fatal("truncation", n)
		}
	}
	for _, bad := range [][]byte{append(bytes.Clone(b), 0), make([]byte, sealLimit+1)} {
		if _, err := verifySeal(bad, key, expected); err == nil {
			t.Fatal("length accepted")
		}
	}
	for _, field := range []int{0, 1, 2, 3, 4} {
		other := expected
		switch field {
		case 0:
			other.Identity.Instance = "another-instance"
		case 1:
			other.Identity.Cluster = "another-cluster"
		case 2:
			other.Identity.Member = "another-member"
		case 3:
			other.Identity.Incarnation = "another-incarnation"
		case 4:
			other.MetadataSchema++
		}
		if _, err := verifySeal(b, key, other); err == nil {
			t.Fatal("reader context mismatch", field)
		}
	}
	if _, err := verifySeal(b, bytes.Repeat([]byte{0x39}, 32), expected); err == nil {
		t.Fatal("wrong key")
	}
}

func TestSealRejectsCorrectlyMACedNoncanonicalFields(t *testing.T) {
	s, key, expected := sealFixture()
	b, err := signSeal(s, key)
	if err != nil {
		t.Fatal(err)
	}
	remac := func(b []byte) {
		mac := hmac.New(sha256.New, key)
		mac.Write(b[:len(b)-32])
		copy(b[len(b)-32:], mac.Sum(nil))
	}
	for _, offset := range []int{8, 10, 12, 14, 16} {
		bad := bytes.Clone(b)
		binary.BigEndian.PutUint16(bad[offset:offset+2], math.MaxUint16)
		remac(bad)
		if _, err := verifySeal(bad, key, expected); err == nil {
			t.Fatal("unknown critical version", offset)
		}
	}
	for _, mutate := range []func([]byte){
		func(b []byte) { binary.BigEndian.PutUint16(b[20:22], sealIdentityBytes+1) },
		func(b []byte) { b[22+len(s.Identity.Instance)] = 1 },
		func(b []byte) { b[22] = 0 },
		func(b []byte) { b[18] = 255 },
		func(b []byte) { b[19] = 255 },
		func(b []byte) { binary.BigEndian.PutUint64(b[20+4*(2+sealIdentityBytes):], 0) },
		func(b []byte) {
			binary.BigEndian.PutUint64(b[20+4*(2+sealIdentityBytes)+8*4+32*2+sealNativeBytes+24:], math.MaxUint64)
		},
		func(b []byte) { b[20+4*(2+sealIdentityBytes)+8*4+32*2+32] = 6 },
	} {
		bad := bytes.Clone(b)
		mutate(bad)
		remac(bad)
		if _, err := verifySeal(bad, key, expected); err == nil {
			t.Fatal("noncanonical field accepted")
		}
	}
	// Correctly authenticate a hostile inner root AND the outer envelope. The
	// decoder still rejects duplicate component scope and impossible generation.
	rootAt := 20 + 4*(2+sealIdentityBytes) + 8*4 + 32*2 + sealStores*sealNativeBytes + sealOrderingBytes
	duplicate := bytes.Clone(b)
	copy(duplicate[rootAt+rootBytes:rootAt+2*rootBytes], b[rootAt:rootAt+rootBytes])
	remac(duplicate)
	if _, err := verifySeal(duplicate, key, expected); err == nil {
		t.Fatal("two independently valid roots used the same component")
	}
	for _, offset := range []int{rootAt + rootBytes + 10, rootAt + 14} {
		bad := bytes.Clone(b)
		if offset == rootAt+14 {
			binary.BigEndian.PutUint64(bad[offset:], 0)
		} else {
			binary.BigEndian.PutUint16(bad[offset:], 1)
		}
		start := rootAt
		if offset >= rootAt+rootBytes {
			start += rootBytes
		}
		inner := hmac.New(sha256.New, key)
		inner.Write(bad[start : start+rootBytes-32])
		copy(bad[start+rootBytes-32:start+rootBytes], inner.Sum(nil))
		remac(bad)
		if _, err := verifySeal(bad, key, expected); err == nil {
			t.Fatal("correctly MACed invalid component accepted", offset)
		}
	}
}

func TestSealRejectsMissingComponentsAndInvalidNativeCuts(t *testing.T) {
	s, key, _ := sealFixture()
	cases := []func(*stateSeal){
		func(s *stateSeal) { s.Roots[0] = root{} },
		func(s *stateSeal) { s.Roots[1] = s.Roots[0] },
		func(s *stateSeal) { s.Roots[4].scope = 99 },
		func(s *stateSeal) { s.Roots[0].revision++ },
		func(s *stateSeal) { s.Roots[0].count = 1 },
		func(s *stateSeal) { s.Native[0].Generation = 0 },
		func(s *stateSeal) { s.Native[1].FileRevision = 0 },
		func(s *stateSeal) { s.Native[1].Offset = math.MaxUint64 },
		func(s *stateSeal) { s.Native[2].Sequence = 1 },
		func(s *stateSeal) { s.Native[0].Epoch = 6 },
		func(s *stateSeal) { s.MetadataBaselineRoot = [32]byte{} },
		func(s *stateSeal) { s.Identity.Instance = string([]byte{0xff}) },
		func(s *stateSeal) { s.Ordering.Term = 1 },
	}
	for i, mutate := range cases {
		bad := s
		mutate(&bad)
		if _, err := signSeal(bad, key); err == nil {
			t.Fatal("invalid claim accepted", i)
		}
	}
}

func TestSealHAOrderingAndLegacyCoverageAreExplicit(t *testing.T) {
	s, key, _ := sealFixture()
	s.Identity.Cluster, s.Identity.Member, s.Identity.Incarnation = "cluster", "member", "incarnation"
	s.Role = sealPrimary
	s.PublisherTerm = 5
	s.Ordering = sealOrderingCut{HeaderDigest: sha256.Sum256([]byte("header")), BaselineDigest: sha256.Sum256([]byte("baseline")), LedgerRollDigest: sha256.Sum256([]byte("no-pending-roll"))}
	for _, role := range []uint8{sealPrimary, sealReplica} {
		s.Role = role
		for _, index := range []uint64{0, 7} {
			s.Ordering.Index = index
			if index > 0 {
				s.Ordering.Offset = 1024
				s.Ordering.Term = 3
				s.Ordering.MAC = sha256.Sum256([]byte("ordering-mac"))
			}
			b, err := signSeal(s, key)
			if err != nil {
				t.Fatal(err)
			}
			actual, err := verifySeal(b, key, sealReaderContext{s.Identity, 45})
			if err != nil || actual != s {
				t.Fatal(err)
			}
			// A valid old member claim is never transferable to a peer's identity.
			other := s.Identity
			other.Member = "other-member"
			if _, err := verifySeal(b, key, sealReaderContext{other, 45}); err == nil {
				t.Fatal("foreign member accepted")
			}
		}
		s.Ordering.Index, s.Ordering.Offset, s.Ordering.Term = 0, 0, 0
		s.Ordering.MAC = [32]byte{}
	}
	for _, coverage := range []uint8{sealMixed, sealLegacy} {
		s.Coverage = coverage
		s.Native[0].Epoch = 3
		s.Native[0].Sequence = 10
		s.Native[0].Offset = 4096
		b, err := signSeal(s, key)
		if err != nil {
			t.Fatal(err)
		}
		actual, err := verifySeal(b, key, sealReaderContext{s.Identity, 45})
		if err != nil || actual.Coverage != coverage {
			t.Fatal("coverage promoted", err)
		}
	}
	s.PublisherTerm = 2
	s.Ordering.Index, s.Ordering.Offset, s.Ordering.Term = 1, 1024, 3
	s.Ordering.MAC = sha256.Sum256([]byte("ordering-mac"))
	if _, err := signSeal(s, key); err == nil {
		t.Fatal("ordering term newer than publisher")
	}
	s.PublisherTerm = 5
	s.Coverage = sealAuthenticated
	if _, err := signSeal(s, key); err == nil {
		t.Fatal("CRC-only head given authenticated coverage")
	}
}

func TestSealSavedLedgerKeySurvivesMasterRewrapAndSeparatesPurposes(t *testing.T) {
	ledgerKey := bytes.Repeat([]byte{0x51}, 32)
	master1, master2 := bytes.Repeat([]byte{0x61}, 32), bytes.Repeat([]byte{0x71}, 32)
	const envelopeID = "projection-seal-test"
	v1, err := vault.New(master1)
	if err != nil {
		t.Fatal(err)
	}
	defer v1.Close()
	v2, err := vault.New(master2)
	if err != nil {
		t.Fatal(err)
	}
	defer v2.Close()
	envelope, err := v1.EncryptCredential(envelopeID, "ledger", "internal", ledgerKey)
	if err != nil {
		t.Fatal(err)
	}
	plain, err := v1.DecryptCredential(envelopeID, "ledger", "internal", envelope)
	if err != nil {
		t.Fatal(err)
	}
	rewrapped, err := v2.EncryptCredential(envelopeID, "ledger", "internal", plain)
	clear(plain)
	if err != nil {
		t.Fatal(err)
	}
	reopened, err := v2.DecryptCredential(envelopeID, "ledger", "internal", rewrapped)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(reopened)
	k1, err := deriveSealKey(ledgerKey)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(k1)
	k2, err := deriveSealKey(reopened)
	if err != nil || !bytes.Equal(k1, k2) {
		t.Fatal("rewrap changed seal key", err)
	}
	defer clear(k2)
	native, err := vault.DeriveLedgerHMACKey(master2)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(native)
	gov, err := vault.DeriveGovernanceHMACKey(ledgerKey)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(gov)
	if bytes.Equal(k1, ledgerKey) || bytes.Equal(k1, native) || bytes.Equal(k1, gov) {
		t.Fatal("purpose collision")
	}
	if _, err := deriveSealKey(ledgerKey[:31]); err == nil {
		t.Fatal("short key accepted")
	}
}

// Only the balance component is populated in this reduced fixture. Other roots
// and baseline are synthetic: it proves native-cut binding and page integrity,
// NOT full Ledger/Metadata coverage or authority to skip startup verification.
func TestSealRealLedgerCutAndBalancePageCannotBeMixed(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "ledger.wal")
	nativeKey := bytes.Repeat([]byte{0x81}, 32)
	l, err := ledger.OpenWithOptions(path, ledger.NewStatus(), ledger.Options{ChainKey: nativeKey, MaxBatch: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	base := ledger.Event{EventID: "seal_accepted", Kind: ledger.EventRequestAccepted, RequestID: "req_seal", ProjectID: "prj_seal", KeyID: "key", RequestedModel: "model", PeriodID: "2026-10-04", PeriodTimezone: "UTC", PeriodTimezoneVersion: 1, OccurredAt: time.Date(2026, 10, 4, 1, 0, 0, 0, time.UTC)}
	if _, _, err := l.AppendRecordWithReceipt(ctx, base); err != nil {
		t.Fatal(err)
	}
	base.EventID, base.Kind, base.AttemptID, base.ReservationMicrosUSD = "seal_reserved", ledger.EventReservationCreated, "att_seal", ledger.MicrosUSD(100)
	last, _, err := l.AppendRecordWithReceipt(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	report, partial, err := ledger.InspectReplayAuthenticated(path, nativeKey, nil)
	if err != nil || partial {
		t.Fatal(err)
	}
	projection, err := ledger.BuildLedgerProjection(ctx, path, nativeKey, filepath.Join(t.TempDir(), "projection.db"), report)
	if err != nil {
		t.Fatal(err)
	}
	defer projection.Discard()
	balance, err := projection.ReadBalance(ctx, ledger.BalanceKey{ProjectID: "prj_seal", PeriodID: base.PeriodID, TimezoneVersion: 1})
	if err != nil || balance.ReservedMicrosUSD != 100 {
		t.Fatal(balance, err)
	}
	value, err := json.Marshal(balance)
	if err != nil {
		t.Fatal(err)
	}
	source := &fixtureSource{pages: make(map[uint64][]byte)}
	ref := source.add(t, page{scope: 1, kind: leafKind, count: 1, entries: []entry{{[]byte("balance/prj_seal"), value}}})
	s, _, expected := sealFixture()
	key, err := deriveSealKey(nativeKey)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(key)
	s.Roots[0] = root{scope: 1, generation: 1, revision: 1, height: 1, count: 1, ref: ref}
	s.Native[0].Sequence, s.Native[0].Offset, s.Native[0].Hash, s.Native[0].Epoch = last.Sequence, uint64(last.Offset), last.Hash, last.Epoch
	encoded, err := signSeal(s, key)
	if err != nil {
		t.Fatal(err)
	}
	verified, err := verifySeal(encoded, key, expected)
	if err != nil {
		t.Fatal(err)
	}
	r := reader{source, verified.Roots[0]}
	actual, found, err := r.get(ctx, []byte("balance/prj_seal"))
	if err != nil || !found || !bytes.Equal(actual, value) {
		t.Fatal(found, err)
	}
	// The authentic old seal remains a historical cut after a later real fact;
	// it cannot substitute for replaying the suffix or current role eligibility.
	base.EventID, base.AttemptID = "seal_later", "att_later"
	later, _, err := l.AppendRecordWithReceipt(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	if verified.Native[0].Sequence >= later.Sequence {
		t.Fatal("old cut advanced")
	}
	seen := 0
	head, err := l.ReplayAuthenticated(ctx, ledger.ReplayAnchor{Head: report.Head, Hash: last.Hash, Epoch: last.Epoch}, func(actual ledger.Record) error {
		seen++
		if actual.Sequence != later.Sequence {
			t.Fatal("wrong suffix")
		}
		return nil
	})
	if err != nil || seen != 1 || head.Sequence != later.Sequence {
		t.Fatal("suffix skipped", seen, head, err)
	}
	source.pages[ref.offset][len(source.pages[ref.offset])-1] ^= 1
	if _, found, err := r.get(ctx, []byte("balance/prj_seal")); err == nil || found {
		t.Fatal("seal blessed changed value")
	}
}

func FuzzSealDecoder(f *testing.F) {
	s, key, expected := sealFixture()
	b, _ := signSeal(s, key)
	f.Add(b)
	f.Add([]byte("HMPSEAL1"))
	f.Fuzz(func(t *testing.T, b []byte) {
		got, err := verifySeal(b, key, expected)
		if err != nil {
			if got != (stateSeal{}) {
				t.Fatal("partial failed seal")
			}
			return
		}
		again, err := signSeal(got, key)
		if err != nil || !bytes.Equal(again, b) {
			t.Fatal("noncanonical success")
		}
	})
}
