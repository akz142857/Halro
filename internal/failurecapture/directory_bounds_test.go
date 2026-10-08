package failurecapture

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCaptureDirectoryHistoryPreservesLookupOrderRetentionAndCancellation(t *testing.T) {
	s, root := newStore(t, func(o *Options) { o.Retain = 1000 * 24 * time.Hour })
	now := s.now()
	for day := 100; day >= 0; day-- {
		stamp := now.Add(-time.Duration(day) * 24 * time.Hour)
		directory := filepath.Join(root, stamp.Format("2006-01-02"))
		if err := os.MkdirAll(directory, 0700); err != nil {
			t.Fatal(err)
		}
		for _, hour := range []int{2, 1} {
			r := record("duplicate")
			r.CapturedAt, r.Outcome = stamp.Add(time.Duration(hour)*time.Hour), fmt.Sprintf("day_%d_hour_%d", day, hour)
			raw, err := json.Marshal(r)
			if err != nil {
				t.Fatal(err)
			}
			sealed, err := (fakeSealer{}).EncryptFailurePayload(r.RequestID, r.ProjectID, raw)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(directory, captureName(r.CapturedAt, r.RequestID, r.ProjectID)), sealed, 0600); err != nil {
				t.Fatal(err)
			}
		}
	}
	current := filepath.Join(root, now.Format("2006-01-02"))
	for n := 0; n < 1000; n++ {
		if err := os.WriteFile(filepath.Join(current, fmt.Sprintf("foreign-%d", n)), nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
	r, found, err := s.GetContext(context.Background(), "duplicate", "project_1")
	if err != nil || !found || r.Outcome != "day_0_hour_1" {
		t.Fatal("newest day/first filename semantics changed", r.Outcome, found, err)
	}
	if count, err := countCaptures(context.Background(), current, now.Format("2006-01-02")); err != nil || count != 2 {
		t.Fatal(count, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := s.ProjectOfContext(ctx, "duplicate"); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, _, err := s.GetContext(ctx, "duplicate", "project_1"); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if err := s.PurgeContext(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	s.now = func() time.Time { return now.Add(1002 * 24 * time.Hour) }
	if err := s.PurgeContext(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, found, err := s.Get("duplicate", "project_1"); err != nil || found {
		t.Fatal(found, err)
	}
	if _, err := os.Stat(filepath.Join(current, "foreign-0")); err != nil {
		t.Fatal("unknown entry removed", err)
	}
	if _, err := os.Stat(filepath.Join(root, now.Add(-24*time.Hour).Format("2006-01-02"))); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("empty expired day survived", err)
	}
}

func TestCaptureMetadataAndCiphertextRefuseBeforeUnboundedAllocation(t *testing.T) {
	s, root := newStore(t, nil)
	r := record("wide")
	r.Outcome = strings.Repeat("x", captureMetadataBytes+1)
	if written, err := s.Put(r); written || err == nil {
		t.Fatal("wide metadata reached encoding", written, err)
	}
	directory := filepath.Join(root, s.now().Format("2006-01-02"))
	if err := os.MkdirAll(directory, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, captureName(s.now(), "wide", "project_1"))
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(captureFileCeiling(s.maxBytes) + 1); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if _, found, err := s.Get("wide", "project_1"); found || err == nil {
		t.Fatal("oversized ciphertext reached read/decode", found, err)
	}
	if err := os.Rename(path, path+".owned"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(path+".owned", path); err != nil {
		t.Fatal(err)
	}
	if _, found, err := s.Get("wide", "project_1"); found || err == nil {
		t.Fatal("symlink ciphertext accepted", found, err)
	}
}
