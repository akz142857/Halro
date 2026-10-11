//go:build linux

package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/resourcebudget"
	"golang.org/x/sys/unix"
)

// TestHAPhysicalBackgroundWritersAtENOSPC is the HA half of board §36 on the
// strict pair: the background writers an HA member has, run at actual ENOSPC
// while an accepted request holds its prepaid terminal, must complete or
// defer without a recovery latch, a Ledger append, retired debt or an owned
// temporary on any member, and the accepted terminal must still complete.
func TestHAPhysicalBackgroundWritersAtENOSPC(t *testing.T) {
	exerciseHAPhysicalBackgroundWritersAtENOSPC(t, 2)
}

// The target three-member topology (§34/§35 fixture), same gates.
func TestHAPhysicalThreeMemberBackgroundWritersAtENOSPC(t *testing.T) {
	exerciseHAPhysicalBackgroundWritersAtENOSPC(t, 3)
}

// The writers are the ones an HA member actually has. The HA selected Runtime
// starts no Usage maintenance loop (startHASelectedPrimaryWorkers), so there
// is no scheduled seal, Usage date cycle or common checkpoint on HA; what
// remains is the Primary's Usage collector, the operator's Governance export
// and the online checkpoint each member owns. The Primary's Ledger Roll has
// no HA caller and runs at ENOSPC only, where it must refuse; it is not run
// after space returns, where a roll would exercise an unreachable path.
// Public HA physical startup is still refused (ha_selected_application.go), so
// this is foundation evidence, as §31–§35 are.
func exerciseHAPhysicalBackgroundWritersAtENOSPC(t *testing.T, members int) {
	if os.Getenv("HALRO_HA_SUSTAINED_PRESSURE") != "1" {
		t.Skip("opt-in HA reserve experiment")
	}
	root := os.Getenv("HALRO_EXT4_ROOT")
	if root == "" {
		t.Skip("requires explicitly isolated ext4 runner")
	}
	var fs unix.Statfs_t
	if err := unix.Statfs(root, &fs); err != nil || fs.Type != unix.EXT4_SUPER_MAGIC {
		t.Fatal("actual ext4 fixture", err)
	}
	policy := selectedCapacityPressure()
	profile := ledger.HASelectedCompletionWorkspaceRequirements()
	var completionRAM int64
	for _, slot := range profile {
		completionRAM += slot.RAM
	}
	var (
		ctx     context.Context
		configs []config.Config
		master  []byte
	)
	switch members {
	case 2:
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		fixture := activateHAPhysicalBusinessFixture(t, ctx, true)
		configs, master = fixture.configs[:], fixture.keys.Master
	case 3:
		var fixture haThreeGroupFixture
		ctx, fixture, _ = prepareHAPhysicalThreeMemberFixture(t, 8*time.Minute)
		configs, master = fixture.configs[:], fixture.keys.Master
	default:
		t.Fatal("unsupported member count", members)
	}
	group := openHAPhysicalBusinessGroup(t, ctx, configs, master, true, &policy, nil)
	primary, replicas := group.owners[0], group.owners[1:]
	c := primary.reopened.Owners.Components
	// The operator's export runs through the Runtime function the admin
	// handler calls, on the owners the HA application wires into it.
	exporter := &Runtime{config: primary.cfg, store: primary.metadataStore, accounting: primary.business.accounting}
	exporter.governance.manager = primary.business.governance
	exportRoot := primary.cfg.GovernanceExportPath()
	exportAt := func(name string) error {
		_, err := exporter.writeGovernanceExport(ctx, filepath.Join(exportRoot, name))
		return err
	}
	fills := []string{filepath.Join(root, "foreign-background-fill"), filepath.Join(root, "foreign-background-refill"), filepath.Join(root, "foreign-background-idle-fill")}
	t.Cleanup(func() {
		for _, fill := range fills {
			_ = os.Remove(fill)
		}
	})

	accept := func(name string) (context.Context, func() error, *resourcebudget.CompletableWork, string, func() error) {
		admitted, retire, err := primary.peerPhysicalTerminalAdmission(ctx, 2)
		if err != nil {
			t.Fatal("admission with space", name, err)
		}
		work, err := c.Budget.TryAcquireCompletableWork(resourcebudget.Active, resourcebudget.Cost{RAM: 4096}, resourcebudget.Cost{RAM: completionRAM}, profile)
		if err != nil {
			t.Fatalf("accepted request completion RAM %s: %v %+v", name, err, c.Budget.Snapshot())
		}
		request, err := primary.business.accounting.BeginRequestDetailed(work.Bind(admitted), "prj_physical", "key_physical", name, "model")
		if err != nil {
			t.Fatal("RequestAccepted", name, err)
		}
		finalize := func() error {
			return work.WithWorkspace(admitted, func(ctx context.Context) error {
				return primary.business.accounting.Finalize(ctx, request, "success")
			})
		}
		return admitted, retire, work, request.RequestID, finalize
	}

	_, retire, work, acceptedID, finalize := accept("background-accepted")
	waitHASustainedApplied(t, ctx, primary, replicas, 0)
	type memberState struct {
		debt  int
		head  ledger.Watermark
		hash  [32]byte
		temps []string
	}
	snapshot := func() []memberState {
		states := make([]memberState, members)
		for i, f := range group.owners {
			l := f.reopened.Owners.Components.NativeLedger
			head, hash, _ := l.ChainHead()
			states[i] = memberState{l.PhysicalTerminalDebt(), head, hash, haPhysicalOwnedTemporaries(t, f.cfg.Storage.DataDir)}
		}
		return states
	}
	base := snapshot()
	t.Logf("HA_BACKGROUND_DEBT %v", func() []int {
		debts := make([]int, members)
		for i, s := range base {
			debts[i] = s.debt
		}
		return debts
	}())
	if base[0].debt == 0 {
		t.Fatal("accepted terminal is not prepaid on the Primary; the reserve gate would not hold")
	}
	if err := exhaustHAPhysicalFixture(fills[0], haPhysicalFixtureFillBound); err != nil {
		t.Fatal(err)
	}
	waitHASampledPressure(t, ctx, group.owners, resourcebudget.Pressured, haSampledEnterWithin, 0)

	type writer struct {
		name string
		run  func() error
	}
	writers := []writer{
		{"primary-usage-collector", func() error { return primary.business.collector.CatchUp(ctx) }},
		{"primary-ledger-roll", func() error {
			result, err := c.NativeLedger.Roll()
			if err == nil && result.Rolled {
				return errHABackgroundRolled
			}
			return err
		}},
		{"primary-governance-export", func() error { return exportAt("gex_at_enospc") }},
	}
	for i, f := range group.owners {
		writers = append(writers, writer{fmt.Sprintf("checkpoint-%d", i), func() error {
			checkpointCtx, stop := context.WithTimeout(ctx, 5*time.Second)
			defer stop()
			return f.checkpointHAPhysicalOnline(checkpointCtx)
		}})
	}
	var outcomes []backgroundWriterOutcome
	for _, w := range writers {
		if err := unix.Statfs(root, &fs); err != nil || fs.Bavail != 0 {
			// A writer that freed blocks would let the next run off-pressure.
			// v2: each refill is a file of its own; the helper creates, never
			// appends.
			refill := filepath.Join(root, "foreign-background-refill-"+w.name)
			fills = append(fills, refill)
			if err := exhaustHAPhysicalFixture(refill, haPhysicalFixtureFillBound); err != nil {
				t.Fatal("refill before", w.name, err)
			}
		}
		runErr := w.run()
		outcomes = append(outcomes, backgroundWriterOutcome{w.name, runErr})
		t.Logf("HA_BACKGROUND_AT_ENOSPC writer=%s err=%v", w.name, runErr)
		if errors.Is(runErr, errHABackgroundRolled) {
			t.Fatal("Primary Ledger rolled at ENOSPC beside an accepted terminal")
		}
		after := snapshot()
		for i, f := range group.owners {
			if f.recovery.Load() || f.reopened.Owners.Components.NativeLedger.Status().Load() != ledger.AccountingHealthy {
				t.Fatal(w.name, "at ENOSPC latched recovery on member", i, runErr)
			}
			if after[i].debt != base[i].debt {
				t.Fatal(w.name, "at ENOSPC changed prepaid terminal debt on member", i, base[i].debt, after[i].debt)
			}
			if after[i].head != base[i].head || after[i].hash != base[i].hash {
				t.Fatal(w.name, "at ENOSPC appended to the Ledger on member", i, base[i].head, after[i].head)
			}
			if !slices.Equal(after[i].temps, base[i].temps) {
				t.Fatal(w.name, "at ENOSPC left own temporaries on member", i, after[i].temps, base[i].temps)
			}
		}
		if w.name == "primary-governance-export" && runErr != nil {
			if _, err := os.Lstat(filepath.Join(exportRoot, "gex_at_enospc")); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("failed Governance export left its target", err)
			}
		}
	}

	// The accepted terminal finishes from the reserve at zero free blocks.
	if err := exhaustHAPhysicalFixture(fills[1], haPhysicalFixtureFillBound); err != nil {
		t.Fatal(err)
	}
	if err := unix.Statfs(root, &fs); err != nil || fs.Bavail != 0 {
		t.Fatal("terminal would not run at ENOSPC", fs.Bavail, err)
	}
	terminalStart := time.Now()
	if err := finalize(); err != nil {
		t.Fatal("accepted terminal after background writers at ENOSPC", err)
	}
	if took := time.Since(terminalStart); took > haSustainedTerminalWithin {
		t.Fatal("accepted terminal exceeded its bound", took)
	}
	if err := errors.Join(work.Close(), work.Active.Release()); err != nil {
		t.Fatal("accepted request RAM retirement", err)
	}
	waitHASustainedApplied(t, ctx, primary, replicas, 0)
	if err := retire(); err != nil {
		t.Fatal("accepted completion owners", err)
	}
	if err := exhaustHAPhysicalFixture(fills[2], haPhysicalFixtureFillBound); err != nil {
		t.Fatal(err)
	}
	if _, closeNew, err := primary.peerPhysicalTerminalAdmission(ctx, 2); err == nil {
		_ = closeNew()
		t.Fatal("new admission succeeded at ENOSPC with nothing in flight")
	} else if !errors.Is(err, resourcebudget.ErrPressure) && !errors.Is(err, resourcebudget.ErrPhysicalAllocation) {
		t.Fatal("refusal at ENOSPC is neither the pressure nor the physical stop", err)
	}

	// Space returns. Every writer HA has must now do its work.
	for _, fill := range fills {
		if err := os.Remove(fill); err != nil {
			t.Fatal(err)
		}
	}
	waitHASampledPressure(t, ctx, group.owners, resourcebudget.Healthy, haSampledExitWithin, 0)
	for i, replica := range replicas {
		if _, found, err := replica.reopened.Owners.Components.Ledger.ReadFinalization(ctx, acceptedID); err != nil || !found {
			t.Fatal("Replica did not apply the accepted terminal", i, found, err)
		}
	}
	if err := primary.business.collector.CatchUp(ctx); err != nil {
		t.Fatal("Primary collector after space returned", err)
	}
	// HA selected has no Governance export source either (board §36 v8): it
	// must refuse cleanly, not half-publish.
	if err := exportAt("gex_after_space"); !errors.Is(err, ledger.ErrGovernanceExportBackend) {
		t.Fatal("HA Governance export after space returned did not refuse cleanly", err)
	}
	if _, err := os.Lstat(filepath.Join(exportRoot, "gex_after_space")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("refused Governance export left a target", err)
	}
	deferrals, longest := checkpointHASustained(t, ctx, group.owners, 0)
	waitHAPhysicalCreditWindow(t, ctx, primary, len(replicas))
	_, retireAfter, workAfter, afterID, finalizeAfter := accept("background-after-space")
	if err := finalizeAfter(); err != nil {
		t.Fatal("request after space returned", err)
	}
	if err := errors.Join(workAfter.Close(), workAfter.Active.Release()); err != nil {
		t.Fatal(err)
	}
	waitHASustainedApplied(t, ctx, primary, replicas, 1)
	if err := retireAfter(); err != nil {
		t.Fatal(err)
	}
	for i, f := range group.owners {
		if f.recovery.Load() || f.reopened.Owners.Components.NativeLedger.Status().Load() != ledger.AccountingHealthy {
			t.Fatal("member not healthy after space returned", i)
		}
		if err := f.finishHAPhysicalBusiness(ctx); err != nil {
			t.Fatal("confirmed terminal whole common", i, err)
		}
		if err := f.closeHAPhysicalNative(ctx); err != nil {
			t.Fatal("physical native close", i, err)
		}
	}
	group.release()
	for i, cfg := range configs {
		if left := haPhysicalOwnedTemporaries(t, cfg.Storage.DataDir); len(left) != 0 {
			t.Fatal("owned temporaries after close", i, left)
		}
	}
	reopened := openHAPhysicalBusinessGroup(t, ctx, configs, master, true, &policy, nil)
	for i, f := range reopened.owners {
		for _, id := range []string{acceptedID, afterID} {
			if _, found, err := f.reopened.Owners.Components.Ledger.ReadFinalization(ctx, id); err != nil || !found {
				t.Fatal("reopened member lost a terminal", i, id, found, err)
			}
		}
	}
	for i, f := range reopened.owners {
		if err := f.finishHAPhysicalBusiness(ctx); err != nil {
			t.Fatal("reopened whole common", i, err)
		}
		if err := f.closeHAPhysicalNative(ctx); err != nil {
			t.Fatal("reopened native close", i, err)
		}
	}
	reopened.release()
	summary := ""
	for _, o := range outcomes {
		summary += fmt.Sprintf(" %s=%v", o.name, o.err)
	}
	t.Logf("HA_BACKGROUND_SUMMARY members=%d deferrals=%d checkpoint_max=%s%s", members, deferrals, longest, summary)
}

var errHABackgroundRolled = errors.New("Ledger rolled")
