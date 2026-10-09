//go:build linux

package app

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/replication"
)

// A credit reply lost with its session is resolved by the Replica's answer
// to the identical request on its next session, never by inference: once
// when the reserve was persisted before the session ended, once when it was
// not, and once after the exchange's bound, when the next reserve finishes
// the abandoned ticket. The restart must serve every fact on the Replica.
func TestHAPhysicalLostCreditRepliesResolveByResend(t *testing.T) {
	ctx, fixture, logger := prepareHAPhysicalOnlineFixture(t, 4*time.Minute)
	written, revisions := runHAPhysicalOnlineGeneration(t, ctx, fixture.configs[:], fixture.keys.Master, logger, 0, nil, nil, haPhysicalGenerationLostReplies)
	runHAPhysicalOnlineGeneration(t, ctx, fixture.configs[:], fixture.keys.Master, logger, 1, written, revisions, haPhysicalGenerationNormal)
}

// haPeerPhysicalLostReplies ends the Replica's session at a chosen point of
// a reserve and counts how often each ticket's reserve arrived.
type haPeerPhysicalLostReplies struct {
	mu       sync.Mutex
	stage    string // "received" or "answered"; empty passes everything
	once     bool   // clear stage after the first fault
	arrivals map[[16]byte]int
}

func (l *haPeerPhysicalLostReplies) set(stage string, once bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.stage, l.once = stage, once
}

func (l *haPeerPhysicalLostReplies) count(ticket [16]byte) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.arrivals[ticket]
}

func (l *haPeerPhysicalLostReplies) observe(stage string, record replication.PhysicalCreditRecord) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if stage == "received" && record.Operation == replication.PhysicalCreditReserve {
		if l.arrivals == nil {
			l.arrivals = make(map[[16]byte]int)
		}
		l.arrivals[record.TicketID]++
	}
	if l.stage != stage || record.Operation != replication.PhysicalCreditReserve && record.Operation != replication.PhysicalCreditReserved {
		return nil
	}
	if l.once {
		l.stage = ""
	}
	return fmt.Errorf("injected session end at %s", stage)
}

func exerciseHAPhysicalLostReplies(t *testing.T, ctx context.Context, q *haPeerPhysicalController, primary *haSelectedRuntimeFoundation, replicas []*haSelectedRuntimeFoundation, generation int, cost replication.PhysicalCreditCost, faults *haPeerPhysicalLostReplies) []string {
	t.Helper()
	var ids []string
	write := func(name string, ticket *haPeerPhysicalTicket, capCtx context.Context) {
		t.Helper()
		id := fmt.Sprintf("prj_lost_reply_%d_%s", generation, name)
		if _, err := primary.metadataStore.PutProject(capCtx, domain.Project{ID: id, Name: "lost reply", Enabled: true}, 0, nil); err != nil {
			t.Fatal("fact after resolved reserve", name, err)
		}
		if err := q.finishTicket(ctx, ticket); err != nil {
			t.Fatal("finish after resolved reserve", name, err)
		}
		for r, replica := range replicas {
			if got, err := replica.metadataStore.GetProject(ctx, id); err != nil || got.ID != id {
				t.Fatal("Replica fact after resolved reserve", name, r, err)
			}
		}
		ids = append(ids, id)
	}
	for n, stage := range []string{"answered", "received"} {
		faults.set(stage, true)
		started := time.Now()
		capCtx, ticket, err := q.reserve(ctx, sha256.Sum256([]byte{byte(generation), byte(150 + n)}), cost)
		if err != nil {
			t.Fatal("reserve with a reply lost at", stage, err)
		}
		if got := faults.count(ticket.messages[0].TicketID); got != 2 {
			t.Fatal("reserve was not resent exactly once on the next session", stage, got)
		}
		t.Log("reserve resolved by resend", stage, time.Since(started))
		write(stage, ticket, capCtx)
	}
	// Every resend is lost until the exchange's bound: the owner returns with
	// an unknown reply and abandons the ticket, which then holds the bank.
	faults.set("received", false)
	_, abandoned, err := q.reserve(ctx, sha256.Sum256([]byte{byte(generation), 160}), cost)
	if !errors.Is(err, context.DeadlineExceeded) || abandoned == nil {
		t.Fatal("reserve past its bound", err)
	}
	q.mu.Lock()
	orphaned := q.live == abandoned && abandoned.orphaned
	q.mu.Unlock()
	if !orphaned {
		t.Fatal("unknown reserve was not left to the next owner")
	}
	faults.set("", false)
	capCtx, ticket, err := q.reserve(ctx, sha256.Sum256([]byte{byte(generation), 161}), cost)
	if err != nil {
		t.Fatal("reserve after the abandoned ticket", err)
	}
	q.mu.Lock()
	resolved := q.live == ticket
	q.mu.Unlock()
	if !resolved {
		t.Fatal("abandoned ticket still live")
	}
	write("after-orphan", ticket, capCtx)
	return ids
}
