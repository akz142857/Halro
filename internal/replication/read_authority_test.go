package replication

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

func memberReadAuthorityFixture(t *testing.T) (*StatePublisher, *resourcebudget.Manager) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "state.json")
	key := bytes.Repeat([]byte{22}, 32)
	state := publisherTestState(RolePrimary)
	if err := WriteState(path, state, key); err != nil {
		t.Fatal(err)
	}
	p, err := NewStatePublisher(path, key, state)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	return p, frozenOrderingBudget(t)
}

func TestMemberReadAuthorityActualSourceAndScopedOncePublication(t *testing.T) {
	p, b := memberReadAuthorityFixture(t)
	var escaped func(func() error) error
	count := 0
	err := p.WithReadAuthority(context.Background(), b, func(ctx context.Context, s MemberState, authorize func(func() error) error) error {
		escaped = authorize
		s.Peers[0].Name = "caller mutation" // Cannot change independent retained eligibility.
		return authorize(func() error { count++; return nil })
	})
	if err != nil || count != 1 {
		t.Fatal("eligible publication rejected", count, err)
	}
	if err := escaped(func() error { count++; return nil }); !errors.Is(err, ErrMemberAuthority) || count != 1 {
		t.Fatal("escaped barrier authorized", count, err)
	}
	if b.Snapshot().Used != (resourcebudget.Cost{}) {
		t.Fatal("job fee leaked", b.Snapshot())
	}
}

func TestMemberReadAuthorityIgnoredBarrierFailureCannotReturnSuccess(t *testing.T) {
	for _, shape := range []string{"action-rejected", "missing", "repeated", "nil"} {
		t.Run(shape, func(t *testing.T) {
			p, b := memberReadAuthorityFixture(t)
			rejection := errors.New("publication refused")
			err := p.WithReadAuthority(context.Background(), b, func(_ context.Context, _ MemberState, authorize func(func() error) error) error {
				switch shape {
				case "action-rejected":
					_ = authorize(func() error { return rejection })
				case "repeated":
					_ = authorize(func() error { return nil })
					_ = authorize(func() error { return nil })
				case "nil":
					_ = authorize(nil)
				}
				return nil
			})
			if err == nil {
				t.Fatal("ignored barrier failure returned success", shape)
			}
			if shape == "action-rejected" && !errors.Is(err, rejection) {
				t.Fatal("lost real publication refusal", err)
			}
			if err := p.WithReadAuthority(context.Background(), b, func(_ context.Context, _ MemberState, authorize func(func() error) error) error {
				return authorize(func() error { return nil })
			}); err != nil {
				t.Fatal("known candidate rejection poisoned member", err)
			}
		})
	}
}

func TestMemberReadAuthorityHigherTermPromiseRevokesWithoutWaitingForScan(t *testing.T) {
	p, b := memberReadAuthorityFixture(t)
	entered, release, canceled := make(chan struct{}), make(chan struct{}), make(chan struct{})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	result := make(chan error, 1)
	count := 0
	go func() {
		result <- p.WithReadAuthority(context.Background(), b, func(ctx context.Context, _ MemberState, authorize func(func() error) error) error {
			close(entered)
			<-ctx.Done()
			close(canceled)
			<-release
			_ = authorize(func() error { count++; return nil })
			return nil
		})
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("job not entered")
	}
	promised := make(chan error, 1)
	go func() { _, err := p.Promise(8); promised <- err }()
	select {
	case err := <-promised:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("long scan blocked fencing")
	}
	select {
	case <-canceled:
	case <-time.After(5 * time.Second):
		t.Fatal("new promise did not revoke")
	}
	state, err := ReadState(p.path, p.key[:])
	if err != nil || state.Role != RoleReplica || state.PromisedTerm != 8 {
		t.Fatal("promise not durable", state, err)
	}
	close(release)
	if err := <-result; !errors.Is(err, context.Canceled) || count != 0 {
		t.Fatal("old tenure published", count, err)
	}
	if b.Snapshot().Used != (resourcebudget.Cost{}) {
		t.Fatal("joined scan fee leaked", b.Snapshot())
	}
}

func TestMemberReadAuthorityProgressChangeRevokesSelectedCut(t *testing.T) {
	p, b := memberReadAuthorityFixture(t)
	count := 0
	err := p.WithReadAuthority(context.Background(), b, func(ctx context.Context, _ MemberState, authorize func(func() error) error) error {
		if err := p.PublishPrimary(PrimaryProgress{DurableIndex: 1, ConfirmedIndex: 1, OrderingHeadMAC: [32]byte{1}, Projection: ProjectionState{Index: 1}}); err != nil {
			return err
		}
		_ = authorize(func() error { count++; return nil })
		return nil
	})
	if !errors.Is(err, context.Canceled) || count != 0 {
		t.Fatal("obsolete cut still published", count, err)
	}
}

func TestMemberReadAuthorityChangedRealFileCannotAuthorizePublication(t *testing.T) {
	for _, shape := range []string{"forged-mac", "valid-old-cut", "symlink"} {
		t.Run(shape, func(t *testing.T) {
			p, b := memberReadAuthorityFixture(t)
			count := 0
			err := p.WithReadAuthority(context.Background(), b, func(_ context.Context, s MemberState, authorize func(func() error) error) error {
				raw, err := os.ReadFile(p.path)
				if err != nil {
					return err
				}
				switch shape {
				case "forged-mac":
					raw[len(raw)-5] ^= 1
					err = os.WriteFile(p.path, raw, 0600)
				case "valid-old-cut":
					s.PromisedTerm++
					err = WriteState(p.path, s, p.key[:])
				case "symlink":
					err = os.Rename(p.path, p.path+".original")
					if err == nil {
						err = os.Symlink(p.path+".original", p.path)
					}
				}
				if err != nil {
					return err
				}
				_ = authorize(func() error { count++; return nil })
				return nil
			})
			if !errors.Is(err, ErrMemberAuthority) || count != 0 {
				t.Fatal("changed actual member file authorized", shape, count, err)
			}
			if _, err := p.Promise(8); err == nil {
				t.Fatal("unknown member source reopened publication")
			}
			if b.Snapshot().Used != (resourcebudget.Cost{}) {
				t.Fatal("known refusal leaked job", b.Snapshot())
			}
		})
	}
}

func TestMemberReadAuthorityCloseCancelsJoinsAndRejectsLaterTransitions(t *testing.T) {
	p, b := memberReadAuthorityFixture(t)
	entered, release, canceled := make(chan struct{}), make(chan struct{}), make(chan struct{})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	result := make(chan error, 1)
	go func() {
		result <- p.WithReadAuthority(context.Background(), b, func(ctx context.Context, _ MemberState, _ func(func() error) error) error {
			close(entered)
			<-ctx.Done()
			close(canceled)
			<-release
			return nil
		})
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("job not entered")
	}
	before := b.Snapshot().Used
	closed := make(chan struct{})
	go func() { p.Close(); close(closed) }()
	select {
	case <-canceled:
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not revoke")
	}
	select {
	case <-closed:
		t.Fatal("Close skipped join")
	case <-time.After(25 * time.Millisecond):
	}
	if b.Snapshot().Used != before {
		t.Fatal("active callback refunded")
	}
	close(release)
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatal("closed authority returned success", err)
	}
	<-closed
	if p.key != ([32]byte{}) || b.Snapshot().Used != (resourcebudget.Cost{}) {
		t.Fatal("known Close retained resources", b.Snapshot())
	}
	if _, err := p.Promise(8); !errors.Is(err, ErrMemberAuthority) {
		t.Fatal("closed publisher promised", err)
	}
	if _, err := p.AdoptHigherTerm(7, 7); !errors.Is(err, ErrMemberAuthority) {
		t.Fatal("closed no-op granted authority", err)
	}
}

func TestMemberReadAuthorityActualReaderCloseDebtRetainsKeyAndFee(t *testing.T) {
	p, b := memberReadAuthorityFixture(t)
	key := p.key
	var actual *os.File
	p.closeAuthorityFile = func(f *os.File) error { actual = f; return errors.Join(f.Close(), f.Close()) }
	called := false
	err := p.WithReadAuthority(context.Background(), b, func(context.Context, MemberState, func(func() error) error) error { called = true; return nil })
	if !errors.Is(err, ownedfile.ErrRetirement) || called || actual == nil {
		t.Fatal("unknown actual source exit authorized", called, actual, err)
	}
	before := b.Snapshot().Used
	p.Close()
	p.Close()
	if p.key != key || b.Snapshot().Used != before || before.RAM < MemberAuthorityRAMBytes {
		t.Fatal("unknown exit erased original resources", p.key, b.Snapshot())
	}
}

func queuedMemberAuthorityBudget(t *testing.T) *resourcebudget.Manager {
	t.Helper()
	limits := resourcebudget.Limits{Total: resourcebudget.Cost{RAM: 8 << 20}, MaxClaims: 32, MaxOwners: 8, MaxPressureAge: time.Hour}
	limits.Classes[resourcebudget.IO] = resourcebudget.Cost{RAM: 6 << 20}
	limits.Classes[resourcebudget.Control] = resourcebudget.Cost{RAM: 32 * resourcebudget.ClaimRAMBytes}
	b, err := resourcebudget.New(limits)
	if err != nil {
		t.Fatal(err)
	}
	if err := b.SetPressure(resourcebudget.Healthy); err != nil {
		t.Fatal(err)
	}
	return b
}

func waitMemberAuthorityJobs(t *testing.T, p *StatePublisher, count int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		p.mu.Lock()
		n := 0
		for _, job := range p.authorityJobs {
			if job != nil {
				n++
			}
		}
		p.mu.Unlock()
		if n == count {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("actual member read queue did not register", count)
}

func TestMemberReadAuthorityQueueCapturesNewPromiseAndCloseJoinsWaiters(t *testing.T) {
	for _, closePublisher := range []bool{false, true} {
		t.Run(map[bool]string{false: "fresh-promise", true: "close-joins"}[closePublisher], func(t *testing.T) {
			p, _ := memberReadAuthorityFixture(t)
			b := queuedMemberAuthorityBudget(t)
			entered, release := make(chan struct{}), make(chan struct{})
			defer func() {
				select {
				case <-release:
				default:
					close(release)
				}
			}()
			first, second := make(chan error, 1), make(chan error, 1)
			go func() {
				first <- p.WithReadAuthority(context.Background(), b, func(ctx context.Context, _ MemberState, authorize func(func() error) error) error {
					close(entered)
					<-release
					return authorize(func() error { return nil })
				})
			}()
			<-entered
			called := make(chan MemberState, 1)
			go func() {
				second <- p.WithReadAuthority(context.Background(), b, func(_ context.Context, s MemberState, authorize func(func() error) error) error {
					called <- s
					return authorize(func() error { return nil })
				})
			}()
			waitMemberAuthorityJobs(t, p, 2)
			if used := b.Snapshot().Used.RAM; used < 2*MemberAuthorityRAMBytes {
				t.Fatal("queued source refunded before join", used)
			}
			select {
			case <-called:
				t.Fatal("queued reader bypassed active job")
			default:
			}
			if closePublisher {
				closed := make(chan struct{})
				go func() { p.Close(); close(closed) }()
				select {
				case err := <-second:
					if !errors.Is(err, context.Canceled) {
						t.Fatal(err)
					}
				case <-time.After(5 * time.Second):
					t.Fatal("queued reader was not canceled")
				}
				select {
				case <-closed:
					t.Fatal("Close skipped active callback join")
				default:
				}
				close(release)
				if err := <-first; !errors.Is(err, context.Canceled) {
					t.Fatal(err)
				}
				<-closed
				select {
				case <-called:
					t.Fatal("closed queue invoked callback")
				default:
				}
			} else {
				if _, err := p.Promise(8); err != nil {
					t.Fatal(err)
				}
				close(release)
				if err := <-first; !errors.Is(err, context.Canceled) {
					t.Fatal("old promise was granted", err)
				}
				if err := <-second; err != nil {
					t.Fatal("queued read refused current authority", err)
				}
				s := <-called
				if s.PromisedTerm != 8 || s.Role != RoleReplica {
					t.Fatal("queue retained stale authority", s)
				}
			}
			if b.Snapshot().Used != (resourcebudget.Cost{}) {
				t.Fatal("queue credits leaked", b.Snapshot())
			}
		})
	}
}

func TestMemberReadAuthorityBoundedQueueOwnsAllCreditsUntilJoin(t *testing.T) {
	p, _ := memberReadAuthorityFixture(t)
	b := queuedMemberAuthorityBudget(t)
	entered, release := make(chan struct{}), make(chan struct{})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	results := make(chan error, 16)
	go func() {
		results <- p.WithReadAuthority(context.Background(), b, func(ctx context.Context, _ MemberState, _ func(func() error) error) error {
			close(entered)
			<-release
			return ctx.Err()
		})
	}()
	<-entered
	for range 15 {
		go func() {
			results <- p.WithReadAuthority(context.Background(), b, func(context.Context, MemberState, func(func() error) error) error {
				t.Error("queued callback ran after Close")
				return nil
			})
		}()
	}
	waitMemberAuthorityJobs(t, p, 16)
	before := b.Snapshot().Used
	if err := p.WithReadAuthority(context.Background(), b, func(context.Context, MemberState, func(func() error) error) error {
		t.Error("overflow callback ran")
		return nil
	}); !errors.Is(err, ErrMemberAuthority) {
		t.Fatal("overflow admitted", err)
	}
	if b.Snapshot().Used != before || before.RAM < 16*MemberAuthorityRAMBytes {
		t.Fatal("queue did not cover complete owners", b.Snapshot(), before)
	}
	closed := make(chan struct{})
	go func() { p.Close(); close(closed) }()
	for range 15 {
		select {
		case err := <-results:
			if !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("Close failed to cancel queue")
		}
	}
	select {
	case <-closed:
		t.Fatal("Close skipped active join")
	default:
	}
	close(release)
	if err := <-results; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	<-closed
	if b.Snapshot().Used != (resourcebudget.Cost{}) {
		t.Fatal("accepted source fee leaked", b.Snapshot())
	}
}
