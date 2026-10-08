package ledger

import (
	"context"
	"os"
	"testing"
	"time"
)

func TestSelectedStartupSourceJoinsActualAuditDescriptorRead(t *testing.T) {
	f := newSelectedStartupFixture(t, true)
	f.reopen(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	entered, release := make(chan struct{}), make(chan struct{})
	audit, err := f.native.startSelectedStartupAudit(ctx, f.o.DirectoryOptions.Budget, StartupAuditOptions{Deadline: time.Second, ProgressTimeout: time.Second}, func(r Record, _ *os.File) {
		if r.Sequence == 1 {
			f.native.mu.Lock()
			close(entered)
			<-release
			f.native.mu.Unlock()
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	<-entered
	done := make(chan error, 1)
	go func() {
		done <- f.native.WithSelectedStartupOwner(ctx, f.o.DirectoryOptions.Budget, SelectedLedgerProjectionOpenOptions{Publisher: f.o.Publisher, Guard: f.o.Guard, Authority: f.o.Authority}, f.o.DirectoryOptions.Identity, func(ctx context.Context, source SelectedStartupSource) error { return source.Check(ctx) })
	}()
	var early error
	finished := false
	select {
	case early = <-done:
		finished = true
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	if finished {
		t.Fatal("actual historical descriptor lookup invalidated startup source", early)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := audit.Wait(ctx); err != nil {
		t.Fatal(err)
	}
}
