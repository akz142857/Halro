package bolt

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/metadatajournal"
	bbolt "go.etcd.io/bbolt"
)

func TestUsageSummaryLabelsReadOnlyDisplayedDimension(t *testing.T) {
	s, err := openForTest(t, filepath.Join(t.TempDir(), "metadata.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	if _, err := s.PutProject(ctx, domain.Project{ID: "prj_shown", Name: "Shown", Enabled: true}, 0, nil); err != nil {
		t.Fatal(err)
	}
	// Faults in unused historical records must not be touched by point reads.
	if err := s.db.Update(func(tx *bbolt.Tx) error {
		for _, b := range [][]byte{bucketProjects, bucketDeployments, bucketProviders} {
			if err := tx.Bucket(b).Put([]byte("unused"), []byte("corrupt")); err != nil {
				return err
			}
		}
		return tx.Bucket(bucketDeployments).Put([]byte("dep_shown"), []byte(`{"id":"dep_shown","name":" Deployment ","binding_id":""}`))
	}); err != nil {
		t.Fatal(err)
	}
	got, err := s.UsageSummaryLabels(ctx, domain.RollupDimensionProject, []string{"prj_shown", "prj_shown", "missing", domain.RollupOtherKey})
	if err != nil || !reflect.DeepEqual(got, map[string]string{"prj_shown": "Shown"}) {
		t.Fatal(got, err)
	}
	got, err = s.UsageSummaryLabels(ctx, domain.RollupDimensionDeployment, []string{"dep_shown"})
	if err != nil || got["dep_shown"] != " Deployment " {
		t.Fatal(got, err)
	}
	got, err = s.UsageSummaryLabels(ctx, domain.RollupDimensionProvider, []string{"unused"})
	if err != nil || len(got) != 0 {
		t.Fatal("provider normalization/list leaked", got, err)
	}
}
func TestUsageSummaryLabelsRefuseWideCorruptAndCanceledSources(t *testing.T) {
	s, err := openForTest(t, filepath.Join(t.TempDir(), "metadata.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	put := func(raw []byte) {
		t.Helper()
		if err := s.db.Update(func(tx *bbolt.Tx) error { return tx.Bucket(bucketProjects).Put([]byte("shown"), raw) }); err != nil {
			t.Fatal(err)
		}
	}
	for _, raw := range [][]byte{[]byte("invalid"), []byte(`{"id":"other","name":"foreign"}`), []byte(strings.Repeat("x", metadatajournal.MaxPayloadSize+1))} {
		put(raw)
		if got, err := s.UsageSummaryLabels(ctx, domain.RollupDimensionProject, []string{"shown"}); err == nil || got != nil {
			t.Fatal("partial label escaped", got, err)
		}
	}
	raw, err := json.Marshal(map[string]string{"id": "shown", "name": strings.Repeat("n", UsageSummaryLabelOutputBytes)})
	if err != nil {
		t.Fatal(err)
	}
	put(raw)
	if got, err := s.UsageSummaryLabels(ctx, domain.RollupDimensionProject, []string{"shown"}); !errors.Is(err, ErrUsageSummaryLabelsLimit) || got != nil {
		t.Fatal("label output cap", err)
	}
	if got, err := s.UsageSummaryLabels(ctx, domain.RollupDimensionProject, make([]string, 102)); !errors.Is(err, ErrUsageSummaryLabelsLimit) || got != nil {
		t.Fatal("label key cap", err)
	}
	if _, err := s.UsageSummaryLabels(ctx, domain.RollupDimensionProject, []string{strings.Repeat("k", UsageSummaryLabelKeyBytes+1)}); !errors.Is(err, ErrUsageSummaryLabelsLimit) {
		t.Fatal(err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := s.UsageSummaryLabels(cancelled, domain.RollupDimensionProject, []string{"shown"}); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	// Individually finite rows can still exceed whole-call source allowance.
	if err := s.db.Update(func(tx *bbolt.Tx) error {
		for _, id := range []string{"one", "two", "three"} {
			raw, err := json.Marshal(map[string]string{"id": id, "name": "small", "unused": strings.Repeat("x", metadatajournal.MaxPayloadSize-128)})
			if err != nil {
				return err
			}
			if err := tx.Bucket(bucketProjects).Put([]byte(id), raw); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if got, err := s.UsageSummaryLabels(ctx, domain.RollupDimensionProject, []string{"one", "two", "three"}); !errors.Is(err, ErrUsageSummaryLabelsLimit) || got != nil {
		t.Fatal("whole source gate", err)
	}
}
