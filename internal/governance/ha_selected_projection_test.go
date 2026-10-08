package governance

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/auditlimits"
	"github.com/akz142857/Halro/internal/authenticatedstate"
)

// The actual file, saved capsule, native constructor and typed root are real.
// Only the package-local authority provider represents the independent member;
// app's two-node TLS test supplies the real Member/Ordering/source counterpart.
func TestHASelectedGovernanceReceivedTailAndActualReadCloseJoin(t *testing.T) {
	ctx := context.Background()
	f := selectedTypedGovFixtureForTest(t, true)
	if err := f.native.Close(); err != nil {
		t.Fatal(err)
	}
	seal, found, err := f.selected.Publisher.Current()
	if err != nil || !found {
		t.Fatal(err)
	}
	seal.Generation++
	seal.Role = authenticatedstate.SealReplica
	seal.Cluster = [32]byte{11}
	seal.Member = [32]byte{12}
	seal.Term = 7
	seal.OrderingTerm = 7
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
	allowed := HASelectedNativePosition{Sequence: f.cache.Head().Records, Hash: f.cache.Head().LastHash}
	block := false
	entered := make(chan struct{})
	exit := make(chan struct{})
	startup := func(ctx context.Context, visit func(context.Context, authenticatedstate.SealEligibility, HASelectedNativePosition) error) error {
		return visit(ctx, a, allowed)
	}
	read := func(ctx context.Context, visit func(context.Context, authenticatedstate.SealEligibility, HASelectedNativePosition) error) error {
		if block {
			close(entered)
			<-ctx.Done()
			close(exit)
			return ctx.Err()
		}
		return visit(ctx, a, allowed)
	}
	n, receipt, err := OpenWithHASelectedSeal(ctx, f.path, f.key, Options{Replica: true, CacheBudget: f.options.Budget, CacheRAMBytes: auditlimits.MinCacheRAMBytes}, HASelectedSealOpenOptions{SelectedSealOpenOptions: SelectedSealOpenOptions{Publisher: f.selected.Publisher, Guard: f.selected.Guard, Cache: f.cache, Identity: f.options.Identity}, WithAuthority: startup, WithReadAuthority: read})
	if err != nil {
		t.Fatal(err)
	}
	f.native = n
	f.options.Native = n
	f.selected.Authority = a
	if receipt.Head.Records != 7 || receipt.Allowed.Records != 6 {
		t.Fatal("actual native tail became the applied prefix", receipt)
	}
	p, reopened, err := OpenHASelectedTypedGovernanceProjection(ctx, f.options, f.selected)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := p.Close(); err != nil {
			t.Error(err)
		}
	}()
	if reopened.NativeHead.Records != 7 || reopened.To.Records != 6 || reopened.SourceVerified != 1 || reopened.AppliedRecords != 0 {
		t.Fatal("excluded tail was reduced", reopened)
	}
	auditor, err := n.StartSelectedStartupAudit(ctx, f.options.Budget, StartupAuditOptions{Deadline: time.Second, ProgressTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if err := auditor.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	if got, found, err := p.ReadOutcome(ctx, "out_00000007"); err != nil || found {
		t.Fatal("received outcome entered ordinary query", got, found, err)
	}
	if _, err := p.AdvanceAuthenticatedFromNative(ctx, n, n.Summary(), f.options.Budget); err != nil {
		t.Fatal(err)
	}
	if _, _, err := p.ReadOutcome(ctx, "out_00000007"); !errors.Is(err, ErrUnavailable) {
		t.Fatal("staged root bypassed applied permission", err)
	}
	head := n.Summary()
	allowed = HASelectedNativePosition{Sequence: head.Records, Hash: head.LastHash}
	if got, found, err := p.ReadOutcome(ctx, "out_00000007"); err != nil || !found || got.Value != "accepted" {
		t.Fatal("applied query failed", got, found, err)
	}
	block = true
	query := make(chan error, 1)
	go func() { _, err := p.ReadHead(context.Background()); query <- err }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("actual ordinary query did not enter provider")
	}
	closed := make(chan error, 1)
	go func() { closed <- p.Close() }()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("Close did not cancel/join actual query authority")
	}
	select {
	case <-exit:
	default:
		t.Fatal("root/RAM retired before actual query callback exit")
	}
	if err := <-query; !errors.Is(err, context.Canceled) {
		t.Fatal("closed query succeeded", err)
	}
	if err := f.creator.AdoptClosedTypedSuccessor(p); err != nil {
		t.Fatal(err)
	}
}
