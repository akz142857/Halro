package governance

import (
	"bytes"
	"context"
	"errors"
	"testing"
)

func TestHAStartupCacheActualReplicaMatchesAuthenticatedFullCut(t *testing.T) {
	f := newStartupCacheFixture(t, 3)
	ctx := context.Background()
	if cache, err := f.native.CaptureHAStartupCache(ctx, f.options.CacheBudget, f.cache.identity, f.native.Summary()); err == nil || cache != nil {
		t.Fatal("Standalone obtained HA cache", cache, err)
	}
	permitted := f.native.Summary()
	if err := f.native.Close(); err != nil {
		t.Fatal(err)
	}
	options := f.options
	options.Replica = true
	native, err := OpenWithOptions(f.path, f.key, options)
	if err != nil {
		t.Fatal(err)
	}
	defer native.Close()
	before := f.options.CacheBudget.Snapshot().Used
	if cache, err := native.CaptureStartupCache(ctx, f.options.CacheBudget, f.cache.identity); err == nil || cache != nil {
		t.Fatal("Replica passed Standalone cache guard", cache, err)
	}
	wrong := permitted
	wrong.Records--
	if cache, err := native.CaptureHAStartupCache(ctx, f.options.CacheBudget, f.cache.identity, wrong); !errors.Is(err, ErrStartupCache) || cache != nil {
		t.Fatal("physical tail entered allowed prefix cache", cache, err)
	}
	if f.options.CacheBudget.Snapshot().Used != before {
		t.Fatal("refusal leaked workspace")
	}
	cache, err := native.CaptureHAStartupCache(ctx, f.options.CacheBudget, f.cache.identity, permitted)
	if err != nil {
		t.Fatal(err)
	}
	defer cache.Close()
	if !bytes.Equal(cache.raw, f.cache.raw) || cache.Head() != permitted {
		t.Fatal("Replica cache changed authenticated codec or prefix")
	}
	if err := cache.Close(); err != nil {
		t.Fatal(err)
	}
	if f.options.CacheBudget.Snapshot().Used != before {
		t.Fatal("actual cache retirement leaked workspace")
	}
}
