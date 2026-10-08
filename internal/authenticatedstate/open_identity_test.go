package authenticatedstate

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestOpenStoreExactIdentityRefusesBeforeOrphanRepair(t *testing.T) {
	owner, o := testStore(t, 8)
	identity, err := owner.RootIdentity(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := owner.Close(); err != nil {
		t.Fatal(err)
	}
	credit, err := owner.DiskReservation()
	if err != nil {
		t.Fatal(err)
	}
	defer credit.Release()
	o.DiskReservation = credit
	path := filepath.Join(owner.dir, "pages")
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	withOrphan := append(bytes.Clone(original), []byte("unpublished-orphan-pages")...)
	if err := os.WriteFile(path, withOrphan, 0600); err != nil {
		t.Fatal(err)
	}
	defer os.WriteFile(path, original, 0600)
	before := o.Budget.Snapshot().Used
	for _, field := range []string{"digest", "revision", "rows", "id"} {
		t.Run(field, func(t *testing.T) {
			wrong := identity
			switch field {
			case "digest":
				wrong.Digest[0] ^= 1
			case "revision":
				wrong.Revision++
			case "rows":
				wrong.Rows++
			case "id":
				wrong.ID[0] ^= 1
			}
			o.ExpectedRoot = &wrong
			next, err := OpenStore(context.Background(), owner.dir, o)
			if !errors.Is(err, ErrRecovery) || next != nil {
				t.Fatal(next, err)
			}
			got, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(got, withOrphan) || o.Budget.Snapshot().Used != before {
				t.Fatal("identity refusal repaired/refunded", err, o.Budget.Snapshot())
			}
		})
	}
	o.ExpectedRoot = &identity
	next, err := OpenStore(context.Background(), owner.dir, o)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := next.RootIdentity(context.Background()); err != nil || got != identity {
		t.Fatal(got, err)
	}
	if err := next.Close(); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(got, original) {
		t.Fatal("qualified orphan cleanup failed", err)
	}
}

func TestOpenStoreSelectedSealRootRefusesBeforeOrphanRepair(t *testing.T) {
	owner, o := testStore(t, 8)
	schema := SealRootSchema{Kind: SealRootState, Component: SealComponentLedger, IndexID: 1, Codec: 1}
	expected, err := owner.SealRootReference(context.Background(), schema)
	if err != nil {
		t.Fatal(err)
	}
	if err := owner.Close(); err != nil {
		t.Fatal(err)
	}
	credit, err := owner.DiskReservation()
	if err != nil {
		t.Fatal(err)
	}
	defer credit.Release()
	o.DiskReservation = credit
	o.ExpectedSealSchema = &schema
	path := filepath.Join(owner.dir, "pages")
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	withOrphan := append(bytes.Clone(original), []byte("unpublished-orphan-pages")...)
	if err := os.WriteFile(path, withOrphan, 0600); err != nil {
		t.Fatal(err)
	}
	defer os.WriteFile(path, original, 0600)
	before := o.Budget.Snapshot().Used
	for _, field := range []string{"digest", "rows", "file-id", "offset", "length", "schema"} {
		t.Run(field, func(t *testing.T) {
			wrong := expected
			switch field {
			case "digest":
				wrong.Digest[0] ^= 1
			case "rows":
				wrong.Rows++
			case "file-id":
				wrong.FileID[0] ^= 1
			case "offset":
				wrong.Offset++
			case "length":
				wrong.Length++
			case "schema":
				wrong.Codec++
			}
			o.ExpectedSealRoot = &wrong
			next, err := OpenStore(context.Background(), owner.dir, o)
			if !errors.Is(err, ErrRecovery) || next != nil {
				t.Fatal(next, err)
			}
			got, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(got, withOrphan) || o.Budget.Snapshot().Used != before {
				t.Fatal("selected root refusal repaired/refunded", err, o.Budget.Snapshot())
			}
		})
	}
	o.ExpectedSealRoot = &expected
	next, err := OpenStore(context.Background(), owner.dir, o)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := next.SealRootReference(context.Background(), schema); err != nil || got != expected {
		t.Fatal(got, err)
	}
	if err := next.Close(); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(got, original) {
		t.Fatal("qualified selected root did not clean orphan tail", err)
	}
}

func TestOpenStoreSelectedEmptySealRoot(t *testing.T) {
	owner, o := testStore(t, 0)
	schema := SealRootSchema{Kind: SealRootState, Component: SealComponentUsage, IndexID: 1, Codec: 1}
	expected, err := owner.SealRootReference(context.Background(), schema)
	if err != nil || !expected.Empty || expected.Rows != 0 {
		t.Fatal(expected, err)
	}
	if err := owner.Close(); err != nil {
		t.Fatal(err)
	}
	credit, err := owner.DiskReservation()
	if err != nil {
		t.Fatal(err)
	}
	defer credit.Release()
	o.DiskReservation = credit
	o.ExpectedSealSchema = &schema
	o.ExpectedSealRoot = &expected
	reopened, err := OpenStore(context.Background(), owner.dir, o)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	got, err := reopened.SealRootReference(context.Background(), schema)
	if err != nil || got != expected {
		t.Fatal(got, err)
	}
}
