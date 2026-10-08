package app

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/durable"
	"github.com/akz142857/Halro/internal/governance"
	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/store/lock"
)

type haSelectedTransportCrashSpec struct {
	Configs [2]config.Config
	Keys    HAMetadataKeys
	Marker  string
	Mode    string
}

func TestHASelectedActualTLSPartialSIGKILLChild(t *testing.T) {
	path := os.Getenv("HALRO_HA_SELECTED_TRANSPORT_CHILD")
	if path == "" {
		t.Skip("private actual SIGKILL child")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var s haSelectedTransportCrashSpec
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatal(err)
	}
	clear(raw)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	var owners [2]*haSelectedRuntimeFoundation
	for i, cfg := range s.Configs {
		guard, err := lock.Acquire(cfg.Storage.DataDir)
		if err != nil {
			t.Fatal(err)
		}
		owners[i], err = openHASelectedRuntimeFoundation(ctx, cfg, s.Keys.Master, guard, functionalHAMetadataManager(t, cfg), logger)
		if err != nil {
			t.Fatal(err)
		}
		if err := owners[i].prepareSelectedServices(ctx); err != nil {
			t.Fatal(err)
		}
		if err := owners[i].prepareSelectedTransport(logger); err != nil {
			t.Fatal(err)
		}
	}
	targetPhase := "ledger-applied"
	if s.Mode == "metadata" {
		targetPhase = "metadata-applied"
	}
	if s.Mode == "governance" {
		targetPhase = "governance-applied"
	}
	owners[1].applyPhase = func(phase string) {
		if phase != targetPhase {
			return
		}
		member := owners[1].replication.publisher.Snapshot()
		if member.AppliedIndex >= member.ConfirmedIndex {
			t.Fatal("kill phase already applied", member)
		}
		_, _, err := owners[1].reopened.Owners.Components.Ledger.ReadPosition(ctx)
		if s.Mode == "" && !errors.Is(err, ledger.ErrProjectionSource) {
			t.Fatal("kill phase query accepted staged root", err)
		}
		if s.Mode == "metadata" {
			if _, err := owners[1].metadataStore.GetProject(ctx, "sigkill-metadata-project"); err == nil {
				t.Fatal("staged Metadata query accepted before original Applied publication")
			}
		}
		if s.Mode == "governance" {
			if _, _, err := owners[1].reopened.Owners.Components.Governance.ReadOutcome(ctx, "out_sigkill"); !errors.Is(err, governance.ErrUnavailable) {
				t.Fatal("staged Governance query accepted before Applied", err)
			}
		}
		file, err := os.OpenFile(s.Marker, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			t.Fatal(err)
		}
		_, writeErr := file.WriteString("ledger-applied-before-member")
		if err := errors.Join(writeErr, file.Sync(), file.Close(), durable.SyncDirectory(filepath.Dir(s.Marker))); err != nil {
			t.Fatal(err)
		}
		if err := syscall.Kill(os.Getpid(), syscall.SIGKILL); err != nil {
			t.Fatal(err)
		}
	}
	for _, owner := range owners {
		go func(f *haSelectedRuntimeFoundation) { _ = f.serveSelectedTransport(ctx) }(owner)
	}
	for !owners[0].replication.manager.PeerConnections()[s.Configs[1].Replication.NodeID] {
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(10 * time.Millisecond):
		}
	}
	if s.Mode == "metadata" {
		if _, err := owners[0].metadataStore.PutProject(ctx, domain.Project{ID: "sigkill-metadata-project", Name: "actual staged Metadata", Enabled: true}, 0, nil); err != nil {
			t.Fatal(err)
		}
		<-ctx.Done()
		t.Fatal("metadata actual kill phase never reached", ctx.Err())
	}
	if s.Mode == "governance" {
		now := time.Now().UTC()
		event := governance.Event{EventID: "gov_sigkill", ProjectID: "prj_sigkill", WorkUnitID: "wku_sigkill", DefinitionID: "odef_result", DefinitionVersion: 1, OutcomeID: "out_sigkill", Value: "accepted", ReporterKeyID: "key_sigkill", ObservedAt: now, IngestedAt: now, Revision: 1, IdempotencyKeyHash: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", RequestFingerprint: "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}
		if _, err := owners[0].reopened.Owners.Components.NativeGovernance.Append(ctx, event); err != nil {
			t.Fatal(err)
		}
		<-ctx.Done()
		t.Fatal("governance actual kill phase never reached", ctx.Err())
	}
	c := owners[0].reopened.Owners.Components
	now := time.Now().UTC()
	event := ledger.Event{EventID: "sigkill-ha-create", Kind: ledger.EventWorkUnitCreated, WorkUnitID: "wku_sigkillha", ProjectID: "project_sigkill", KeyID: "key_sigkill", OccurredAt: now, PeriodID: now.Format("2006-01-02"), PeriodTimezone: "UTC", PeriodTimezoneVersion: 1, PeriodStartMicros: now.Truncate(24 * time.Hour).UnixMicro(), PeriodEndMicros: now.Truncate(24 * time.Hour).Add(24 * time.Hour).UnixMicro(), Operation: "workunit.create", IdempotencyKeyHash: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", RequestFingerprint: "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}
	if _, _, err := c.NativeLedger.AppendApplyRecordWithReceipt(ctx, event, func(ctx context.Context, r ledger.Record) error {
		_, err := c.Ledger.AdvanceOneFromNative(ctx, c.NativeLedger, ledger.ProjectionPrefix{Head: ledger.Watermark{Generation: r.Generation, Sequence: r.Sequence, Offset: r.Offset}, Hash: r.Hash})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	<-ctx.Done()
	t.Fatal("actual kill phase never reached", ctx.Err())
}

func TestHASelectedActualTLSPartialLedgerSIGKILLRecovery(t *testing.T) {
	testHASelectedActualTLSPartialSIGKILLRecovery(t, "")
}
func TestHASelectedActualTLSPartialMetadataSIGKILLRecovery(t *testing.T) {
	testHASelectedActualTLSPartialSIGKILLRecovery(t, "metadata")
}
func TestHASelectedActualTLSPartialGovernanceSIGKILLRecovery(t *testing.T) {
	testHASelectedActualTLSPartialSIGKILLRecovery(t, "governance")
}

func testHASelectedActualTLSPartialSIGKILLRecovery(t *testing.T, mode string) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	fixture, owners, _ := prepareHASelectedTransportFixture(t, ctx)
	for _, owner := range owners {
		if err := owner.Close(); err != nil {
			t.Fatal(err)
		}
		if err := owner.owners.Components.Guard.Close(); err != nil {
			t.Fatal(err)
		}
	}
	dir := t.TempDir()
	s := haSelectedTransportCrashSpec{Configs: fixture.configs, Keys: fixture.keys, Mode: mode, Marker: filepath.Join(dir, "actual-phase")}
	raw, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "spec.json")
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	clear(raw)
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(binary, "-test.run", "^TestHASelectedActualTLSPartialSIGKILLChild$", "-test.timeout=45s")
	cmd.Env = append(os.Environ(), "HALRO_HA_SELECTED_TRANSPORT_CHILD="+path)
	output, err := cmd.CombinedOutput()
	var exited *exec.ExitError
	if !errors.As(err, &exited) {
		t.Fatalf("actual child was not killed: %v %s", err, output)
	}
	status, ok := exited.Sys().(syscall.WaitStatus)
	if !ok || !status.Signaled() || status.Signal() != syscall.SIGKILL {
		t.Fatalf("wrong child termination: %v %s", err, output)
	}
	marker, err := os.ReadFile(s.Marker)
	if err != nil || string(marker) != "ledger-applied-before-member" {
		t.Fatal("unproven actual kill phase", err)
	}
	cfg := fixture.configs[1]
	guard, err := lock.Acquire(cfg.Storage.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer guard.Close()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	before, _, err := readMemberState(cfg, fixture.keys.Master)
	if err != nil || before.AppliedIndex >= before.ConfirmedIndex || before.ConfirmedIndex != before.DurableIndex {
		t.Fatal("SIGKILL falsely advanced Applied", before, err)
	}
	fresh, err := openHASelectedRuntimeFoundation(ctx, cfg, fixture.keys.Master, guard, functionalHAMetadataManager(t, cfg), logger)
	if err != nil {
		t.Fatal("post-SIGKILL selected native prefix qualification", err)
	}
	defer func() {
		if err := fresh.Close(); err != nil {
			t.Error(err)
		}
	}()
	recovered := fresh.replication.publisher.Snapshot()
	if recovered.AppliedIndex != before.ConfirmedIndex || recovered.Projection.Index != before.ConfirmedIndex {
		t.Fatal("original startup applier did not persist whole confirmed cut", recovered)
	}
	if err := fresh.prepareSelectedServices(ctx); err != nil {
		t.Fatal("post-SIGKILL confirmed services", err)
	}
	p := fresh.reopened.Owners.Components
	head, _, err := p.Ledger.ReadPosition(ctx)
	expectedSequence := uint64(1)
	if mode != "" {
		expectedSequence = 0
	}
	if err != nil || head.Sequence != expectedSequence {
		t.Fatal("post-SIGKILL confirmed root query", head, err)
	}
	if mode == "metadata" {
		project, err := fresh.metadataStore.GetProject(ctx, "sigkill-metadata-project")
		if err != nil || project.Name != "actual staged Metadata" {
			t.Fatal("fresh confirmed Metadata did not restore original authenticated row", project, err)
		}
	}
	if mode == "governance" {
		row, found, err := p.Governance.ReadOutcome(ctx, "out_sigkill")
		if err != nil || !found || row.Value != "accepted" {
			t.Fatal("fresh actual confirmed Governance row", row, found, err)
		}
	}
	if _, err := p.Usage.ReadStats(ctx); err != nil {
		t.Fatal("post-SIGKILL old Usage query", err)
	}
	if err := fresh.prepareSelectedTransport(logger); err != nil {
		t.Fatal(err)
	}
	through, err := fresh.replication.applier.ApplyConfirmed(ctx)
	if err != nil || through != before.ConfirmedIndex {
		t.Fatal("post-SIGKILL actual confirmed application", through, err)
	}
	head, hash, err := p.Ledger.ReadPosition(ctx)
	if err != nil || head.Sequence != expectedSequence || expectedSequence > 0 && hash == ([32]byte{}) {
		t.Fatal("post-SIGKILL Ledger query", head, err)
	}
	ref, err := p.Usage.TypedSealReference(ctx)
	if err != nil || ref.Ledger.Anchor.Head != head || ref.Ledger.Anchor.Hash != hash {
		t.Fatal("post-SIGKILL common Ledger/Usage applied source", err)
	}
	after := fresh.replication.publisher.Snapshot()
	if after.AppliedIndex != through || after.Projection.Index != through {
		t.Fatal("post-SIGKILL actual Member publication", after)
	}
}
