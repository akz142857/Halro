package app

import (
	"bytes"
	"context"
	"crypto/rand"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/usage"
)

// blockingReplayer stands in for a Ledger whose replay is long: the first walk
// reports that it started and then waits to be released.
type blockingReplayer struct {
	entered chan struct{}
	release chan struct{}
}

func (b *blockingReplayer) Replay(from ledger.Watermark, _ func(ledger.Record) error) (ledger.Watermark, error) {
	select {
	case b.entered <- struct{}{}:
	default:
	}
	<-b.release
	return from, nil
}

// The first start after an upgrade rebuilds the whole index in the
// background. The maintenance tick's catch-up used to queue behind that build
// for its ten-second timeout and log "usage history catch-up failed" on every
// tick until the build finished.
func TestUsageCatchUpTickLeavesARunningBuildAlone(t *testing.T) {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	store, _, err := usage.OpenHistory(filepath.Join(t.TempDir(), "history.db"), key)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	replayer := &blockingReplayer{entered: make(chan struct{}, 1), release: make(chan struct{})}
	collector, err := usage.NewCollector(usage.NewAggregate(store), replayer, 16)
	if err != nil {
		t.Fatal(err)
	}
	built := make(chan error, 1)
	go func() { built <- collector.Build(context.Background()) }()
	<-replayer.entered

	var logs bytes.Buffer
	runtime := &Runtime{usageCollector: collector, logger: slog.New(slog.NewTextHandler(&logs, nil))}
	ticked := make(chan struct{})
	go func() {
		runtime.catchUpUsageHistory()
		close(ticked)
	}()
	select {
	case <-ticked:
	case <-time.After(2 * time.Second):
		close(replayer.release)
		<-ticked
		t.Fatalf("the tick waited behind the running build; logged: %s", logs.String())
	}
	close(replayer.release)
	if err := <-built; err != nil {
		t.Fatal(err)
	}
	if strings.Contains(logs.String(), "catch-up failed") {
		t.Fatalf("the tick reported a failure while the build ran: %s", logs.String())
	}
}
