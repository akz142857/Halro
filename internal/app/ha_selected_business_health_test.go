package app

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/akz142857/Halro/internal/authenticatedstate"
)

func TestHASelectedRetainedApplyErrorIsVisibleToConcurrentReadiness(t *testing.T) {
	f := &haSelectedRuntimeFoundation{ctx: context.Background()}
	cause := errors.New("startup apply failed")
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for range 200 {
			f.retainApplyErr(cause)
		}
	}()
	go func() {
		defer wg.Done()
		for range 200 {
			_ = f.businessHealthError(context.Background())
		}
	}()
	wg.Wait()
	if err := f.businessHealthError(context.Background()); !errors.Is(err, authenticatedstate.ErrRecovery) || !errors.Is(err, cause) {
		t.Fatal("retained apply failure did not withdraw business health", err)
	}
}
