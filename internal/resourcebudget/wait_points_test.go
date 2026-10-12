package resourcebudget

import (
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
)

// waitPoint is one production call that can block its caller (board §41, A1):
// a queue wait (WorkQueueContext, AcquireWorkLane), a join (zero-argument
// Wait on a WaitGroup or Cond), or a Wait* method that takes arguments.
// Channel operations, select, mutexes and IO are outside this inventory.
type waitPoint struct {
	kind, file, function, call string
	count                      int
	bound                      string
}

// waitPoints names where each wait's bound comes from. "no timer" entries
// have none of their own and say what ends them instead.
var waitPoints = []waitPoint{
	{"join", "alert/dispatcher.go", "(*Dispatcher).Close", "d.wait.Wait", 1, "no timer: joins work already entered; it ends with that work, which may be uncancellable physical IO"},
	{"join", "alert/dispatcher.go", "(*Dispatcher).worker", "fanOut.Wait", 1, "no timer here: joins one event's deliveries, each bounded by the webhook client (not traced in A1)"},
	{"join", "app/accounting_drain.go", "(*Runtime).stopBackgroundWorkContext", "r.backgroundWait.Wait", 1, "no timer: joins work already entered; it ends with that work, which may be uncancellable physical IO"},
	{"join", "app/admin_invocation_targets.go", "(*Runtime).fetchInvocationTargetCatalogs", "wait.Wait", 1, "no timer here: joins the catalog fetches, each bounded by the request ctx (not traced in A1)"},
	{"join", "app/ha_memory_state_foundation.go", "(*haSelectedRuntimeFoundation).Close", "f.callbacks.Wait", 1, "no timer: joins work already entered; it ends with that work, which may be uncancellable physical IO"},
	{"join", "app/ha_memory_state_foundation.go", "(*haSelectedRuntimeFoundation).withSelectedCut", "scope.borrowers.Wait", 1, "no timer: joins borrowers already admitted to the scope; they end with their own work"},
	{"join", "app/ha_peer_physical_maintenance.go", "(*haPeerPhysicalController).visitPhysicalPause", "s.jobs.Wait", 1, "no timer: joins work already entered; it ends with that work, which may be uncancellable physical IO"},
	{"join", "app/ha_terminal_physical_finish.go", "(*haSelectedRuntimeFoundation).finishHAPhysicalBusiness", "f.callbacks.Wait", 1, "no timer: joins work already entered; it ends with that work, which may be uncancellable physical IO"},
	{"join", "audit/log.go", "(*Log).Close", "l.replayWG.Wait", 1, "no timer: joins work already entered; it ends with that work, which may be uncancellable physical IO"},
	{"join", "budget/manager.go", "(*Manager).applyDurableRecord", "m.applyCond.Wait", 1, "caller ctx: context.AfterFunc broadcasts the cond on cancellation"},
	{"join", "budget/manager.go", "(*Manager).beginRunClose", "admission.changed.Wait", 1, "no timer: waits for the Run's admitted reservations to exit"},
	{"join", "deadman/engine.go", "(*Engine).probeTargets", "probes.Wait", 1, "no timer here: joins this tick's probes, each bounded by its check (not traced in A1)"},
	{"join", "deadman/engine.go", "(*Engine).Run", "workers.Wait", 1, "no timer: joins work already entered; it ends with that work, which may be uncancellable physical IO"},
	{"join", "failurecapture/failurecapture.go", "(*Store).Close", "s.writeWait.Wait", 1, "no timer: joins work already entered; it ends with that work, which may be uncancellable physical IO"},
	{"join", "gateway/deferred_response.go", "(*Service).RunDeferredResponses", "engine.wait.Wait", 1, "no timer: joins work already entered; it ends with that work, which may be uncancellable physical IO"},
	{"join", "governance/ha_selected_startup.go", "(*haSelectedStartupProvenance).withScope", "scope.borrowers.Wait", 1, "no timer: joins borrowers already admitted to the scope; they end with their own work"},
	{"join", "governance/log.go", "(*Log).Close", "l.readerWG.Wait", 1, "no timer: joins work already entered; it ends with that work, which may be uncancellable physical IO"},
	{"join", "ledger/ha_selected_startup.go", "(*haSelectedStartupScope).close", "s.joined.Wait", 1, "no timer: joins work already entered; it ends with that work, which may be uncancellable physical IO"},
	{"join", "metadatajournal/log.go", "(*Log).Close", "l.freezeWG.Wait", 1, "no timer: joins work already entered; it ends with that work, which may be uncancellable physical IO"},
	{"join", "metadatastate/ha_terminal_physical_receive.go", "(*Baseline).joinHAPhysicalReceives", "s.receiveJobs.Wait", 1, "no timer: joins work already entered; it ends with that work, which may be uncancellable physical IO"},
	{"join", "replication/confirmed_prefix.go", "(*ReplicaConfirmedPrefix).join", "p.borrowed.Wait", 1, "no timer: joins borrowers already admitted to the scope; they end with their own work"},
	{"join", "replication/connection_manager.go", "(*ConnectionManager).Serve", "wait.Wait", 1, "no timer: joins work already entered; it ends with that work, which may be uncancellable physical IO"},
	{"join", "replication/journal.go", "(*OrderingJournal).Close", "j.freezeWG.Wait", 1, "no timer: joins work already entered; it ends with that work, which may be uncancellable physical IO"},
	{"join", "replication/member_whole_read_scope.go", "(*memberWholeReadScope).revoke", "s.jobs.Wait", 1, "no timer: joins work already entered; it ends with that work, which may be uncancellable physical IO"},
	{"join", "replication/state_publisher.go", "(*StatePublisher).Close", "p.authorityWG.Wait", 1, "no timer: joins work already entered; it ends with that work, which may be uncancellable physical IO"},
	{"join", "resourcebudget/shared_completable_workspace.go", "(*SharedCompletableWorkspace).visit", "n.jobs.Wait", 1, "no timer: joins work already entered; it ends with that work, which may be uncancellable physical IO"},
	{"join", "statecut/ha_confirmed_metadata_scope.go", "visitHAConfirmedMetadataScope", "scope.borrowers.Wait", 1, "no timer: joins borrowers already admitted to the scope; they end with their own work"},
	{"join", "statecut/ha_selected_authority.go", "withHASelectedCutRecoveryMode", "scope.borrowers.Wait", 1, "no timer: joins borrowers already admitted to the scope; they end with their own work"},
	{"join", "store/bolt/store.go", "(*Store).CloseContext", "l.changed.Wait", 1, "no timer: joins work already entered; it ends with that work, which may be uncancellable physical IO"},
	{"join", "usage/collector.go", "(*Collector).catchUpInPausedWorkspace", "jobs.Wait", 1, "no timer: joins work already entered; it ends with that work, which may be uncancellable physical IO"},
	{"join", "usage/disk_projection.go", "(*DiskProjection).Close", "p.physicalJobs.Wait", 1, "no timer: joins work already entered; it ends with that work, which may be uncancellable physical IO"},
	{"queue", "app/projected_recovery_writer.go", "(*projectedRecoveryWriter).enter", "resourcebudget.WorkQueueContext", 1, "MaxWorkQueueWait (30s) or an earlier caller deadline"},
	{"queue", "budget/authenticated_owner.go", "(authenticatedAccountingOwner).withOwnedAppendWorkspace", "resourcebudget.AcquireWorkLane", 1, "MaxWorkQueueWait (30s) or an earlier caller deadline"},
	{"queue", "ledger/authenticated_business_read.go", "(*AuthenticatedLedgerProjection).admitBusinessRead", "resourcebudget.WorkQueueContext", 1, "MaxWorkQueueWait (30s) or an earlier caller deadline"},
	{"queue", "ledger/maintenance.go", "(*MaintenanceLease).enter", "resourcebudget.WorkQueueContext", 1, "MaxWorkQueueWait (30s) or an earlier caller deadline"},
	{"queue", "metadatastate/facade.go", "(*standaloneFacade).admit", "resourcebudget.WorkQueueContext", 1, "MaxWorkQueueWait (30s) or an earlier caller deadline"},
	{"queue", "replication/read_authority.go", "(*StatePublisher).withReadAuthority", "resourcebudget.AcquireWorkLane", 1, "MaxWorkQueueWait (30s) or an earlier caller deadline"},
	{"queue", "usage/collector.go", "(*Collector).catchUpOwned", "resourcebudget.WorkQueueContext", 1, "MaxWorkQueueWait (30s) or an earlier caller deadline"},
	{"queue", "usage/collector.go", "(*Collector).lockApply", "resourcebudget.WorkQueueContext", 1, "MaxWorkQueueWait (30s) or an earlier caller deadline"},
	{"queue", "usage/typed_rows.go", "(*usageEngine).View", "resourcebudget.AcquireWorkLane", 1, "MaxWorkQueueWait (30s) or an earlier caller deadline"},
	{"wait", "app/command_selected_metadata_read.go", "openSelectedCommandMetadataReadOnly", "a.Wait", 1, "caller ctx, and the startup audit's own startupaudit.MaxDeadline (15m) / ProgressTimeout (<=30s)"},
	{"wait", "app/command_selected_metadata.go", "openSelectedCommandMetadata", "state.generation.WaitStartupAudit", 1, "caller ctx, and the startup audit's own startupaudit.MaxDeadline (15m) / ProgressTimeout (<=30s)"},
	{"wait", "app/ha_group_key_rotation.go", "(*haGroupFileRotation).execute", "r.members[primary].replication.coordinator.WaitConfirmed", 1, "caller ctx; returns early when replication is unavailable"},
	{"wait", "app/ha_memory_state_foundation.go", "(*haSelectedRuntimeFoundation).Close", "auditor.Wait", 1, "audit cancelled first, then joined; it ends within startupaudit.MaxDeadline (15m) / ProgressTimeout (<=30s)"},
	{"wait", "app/ha_metadata_maintenance.go", "(*HAMetadataMaintenance).completeBoundary", "m.member.coordinator.WaitConfirmed", 1, "caller ctx; returns early when replication is unavailable"},
	{"wait", "app/ha_peer_physical_controller.go", "(*haPeerPhysicalController).finishTicket", "q.owner.replication.coordinator.WaitRecoveryAppliedTarget", 1, "caller ctx; returns early when replication is unavailable"},
	{"wait", "app/ha_selected_primary_startup.go", "(*haSelectedRuntimeFoundation).finishHASelectedPrimaryStartupOwned", "f.replication.coordinator.WaitRecoveryTargetWithPeer", 1, "caller ctx; returns early when replication is unavailable"},
	{"wait", "app/ha_selected_transport.go", "(*haSelectedRuntimeFoundation).prepareSelectedRoots", "auditor.Wait", 1, "caller ctx, and the startup audit's own startupaudit.MaxDeadline (15m) / ProgressTimeout (<=30s)"},
	{"wait", "app/ha_terminal_physical_finish.go", "(*haSelectedRuntimeFoundation).finishHAPhysicalBusiness", "f.replication.coordinator.WaitRecoveryAppliedTarget", 1, "caller ctx; returns early when replication is unavailable"},
	{"wait", "app/ha_terminal_physical_online.go", "(*haSelectedRuntimeFoundation).checkpointHAPhysicalOnline", "f.replication.coordinator.WaitRecoveryAppliedTarget", 1, "caller ctx; returns early when replication is unavailable"},
	{"wait", "app/memory_state_resume.go", "ResumeMemoryRuntimeActivation", "s.generation.WaitStartupAudit", 1, "caller ctx, and the startup audit's own startupaudit.MaxDeadline (15m) / ProgressTimeout (<=30s)"},
	{"wait", "app/primary_accounting_startup.go", "(*Runtime).finishPrimaryAccountingStartup", "r.replication.coordinator.WaitRecoveryTarget", 1, "caller ctx; returns early when replication is unavailable"},
	{"wait", "app/primary_startup_completion.go", "(*replicationRuntime).startPrimaryCompletion", "r.coordinator.WaitConfirmed", 1, "caller ctx; returns early when replication is unavailable"},
	{"wait", "app/primary_startup_completion.go", "(*replicationRuntime).startPrimaryCompletion", "r.coordinator.WaitRecoveryAvailability", 2, "caller ctx; returns early when replication is unavailable"},
	{"wait", "app/projected_recovery_writer.go", "(*projectedRecoveryWriter).waitRecoveryTarget", "w.replica.coordinator.WaitRecoveryTarget", 1, "caller ctx; returns early when replication is unavailable"},
	{"wait", "app/projected_recovery_writer.go", "(*projectedRecoveryWriter).waitRecoveryTarget", "w.replica.coordinator.WaitRecoveryTargetWithPeer", 1, "caller ctx; returns early when replication is unavailable"},
	{"wait", "app/replication_runtime.go", "(*replicationRuntime).replicateProviderObject", "r.coordinator.WaitConfirmed", 1, "caller ctx; returns early when replication is unavailable"},
	{"wait", "app/runtime_live_checkpoint.go", "(*Runtime).CheckpointMemoryState", "r.selectedState.generation.WaitStartupAudit", 1, "caller ctx, and the startup audit's own startupaudit.MaxDeadline (15m) / ProgressTimeout (<=30s)"},
	{"wait", "app/runtime_selected_checkpoint.go", "checkpointDrainedSelectedState", "s.generation.WaitStartupAudit", 1, "caller ctx, and the startup audit's own startupaudit.MaxDeadline (15m) / ProgressTimeout (<=30s)"},
	{"wait", "app/runtime_selected_state.go", "openSelectedRuntimeGeneration", "s.generation.WaitStartupAudit", 2, "caller ctx, and the startup audit's own startupaudit.MaxDeadline (15m) / ProgressTimeout (<=30s)"},
	{"wait", "app/runtime_usage_startup.go", "(*Runtime).restoreSelectedUsageDiskWindow", "r.selectedState.generation.WaitStartupAudit", 1, "caller ctx, and the startup audit's own startupaudit.MaxDeadline (15m) / ProgressTimeout (<=30s)"},
	{"wait", "app/runtime.go", "(*Runtime).RunWithReady", "r.replication.coordinator.WaitConfirmed", 1, "RunWithReady's shutdownCtx; returns early when replication is unavailable"},
	{"wait", "app/selected_key_rotation_cold_source.go", "qualifySelectedRotationColdSource", "s.generation.WaitStartupAudit", 1, "caller ctx, and the startup audit's own startupaudit.MaxDeadline (15m) / ProgressTimeout (<=30s)"},
	{"wait", "app/selected_key_rotation_resume.go", "resumeSelectedRotationPublication", "s.generation.WaitStartupAudit", 1, "caller ctx, and the startup audit's own startupaudit.MaxDeadline (15m) / ProgressTimeout (<=30s)"},
	{"wait", "app/selected_key_rotation.go", "rotateSelectedKeyWithAuthority", "s.generation.WaitStartupAudit", 1, "caller ctx, and the startup audit's own startupaudit.MaxDeadline (15m) / ProgressTimeout (<=30s)"},
	{"wait", "app/terminal_physical_runtime.go", "prepareStandalonePhysicalTerminal", "selected.generation.WaitStartupAudit", 1, "caller ctx, and the startup audit's own startupaudit.MaxDeadline (15m) / ProgressTimeout (<=30s)"},
	{"wait", "audit/log.go", "(*Log).Close", "auditJob.Wait", 1, "audit cancelled first, then joined; it ends within startupaudit.MaxDeadline (15m) / ProgressTimeout (<=30s)"},
	{"wait", "audit/startup_cache.go", "(*Log).AuditSelectedStartup", "active.Wait", 1, "caller ctx, and the startup audit's own startupaudit.MaxDeadline (15m) / ProgressTimeout (<=30s)"},
	{"wait", "budget/manager.go", "(*Manager).appendApplyRecord", "m.log.WaitConfirmed", 1, "caller ctx; returns early when replication is unavailable"},
	{"wait", "governance/log.go", "(*Log).Close", "auditJob.Wait", 1, "audit cancelled first, then joined; it ends within startupaudit.MaxDeadline (15m) / ProgressTimeout (<=30s)"},
	{"wait", "governance/startup_cache.go", "(*Log).AuditSelectedStartup", "active.Wait", 1, "caller ctx, and the startup audit's own startupaudit.MaxDeadline (15m) / ProgressTimeout (<=30s)"},
	{"wait", "ledger/log.go", "(*Log).CloseContext", "auditJob.Wait", 1, "audit cancelled first, then joined; it ends within startupaudit.MaxDeadline (15m) / ProgressTimeout (<=30s)"},
	{"wait", "ledger/log.go", "(*Log).WaitConfirmed", "l.options.WaitConfirmed", 1, "caller ctx; returns early when replication is unavailable"},
	{"wait", "ledger/selected_startup.go", "(*Log).AuditSelectedStartup", "a.Wait", 1, "caller ctx, and the startup audit's own startupaudit.MaxDeadline (15m) / ProgressTimeout (<=30s)"},
	{"wait", "ledger/selected_startup.go", "(*Log).AuditSelectedStartup", "active.Wait", 1, "caller ctx, and the startup audit's own startupaudit.MaxDeadline (15m) / ProgressTimeout (<=30s)"},
	{"wait", "metadatajournal/log.go", "(*Log).Close", "auditJob.Wait", 1, "audit cancelled first, then joined; it ends within startupaudit.MaxDeadline (15m) / ProgressTimeout (<=30s)"},
	{"wait", "metadatastate/delta.go", "(*Baseline).appendFacadeFrameLocked", "b.journal.WaitConfirmed", 1, "caller ctx; returns early when replication is unavailable"},
	{"wait", "replication/metadata_baseline_bridge.go", "(*PrimaryCoordinator).RecordMetadataBaseline", "c.WaitConfirmed", 2, "caller ctx; returns early when replication is unavailable"},
	{"wait", "statecut/reopen_selected.go", "(*ReopenedStandalone).WaitStartupAudit", "audit.Wait", 1, "caller ctx, and the startup audit's own startupaudit.MaxDeadline (15m) / ProgressTimeout (<=30s)"},
	{"wait", "statecut/reopen_selected.go", "ReopenSelectedStandalone", "audit.WaitReady", 1, "caller ctx, and the startup audit's own startupaudit.MaxDeadline (15m) / ProgressTimeout (<=30s)"},
	{"wait", "statecut/reopen_selected.go", "ReopenSelectedStandalone", "r.startupAudits[0].Wait", 1, "caller ctx, and the startup audit's own startupaudit.MaxDeadline (15m) / ProgressTimeout (<=30s)"},
	{"wait", "statecut/reopen.go", "(*ReopenedStandalone).closeWithNative", "audit.Wait", 1, "audit cancelled first, then joined; it ends within startupaudit.MaxDeadline (15m) / ProgressTimeout (<=30s)"},
	{"wait", "statecut/services.go", "(*ReopenedStandalone).StartSelectedServices", "audit.Wait", 1, "caller ctx, and the startup audit's own startupaudit.MaxDeadline (15m) / ProgressTimeout (<=30s)"},
	{"wait", "statecut/services.go", "(*ReopenedStandalone).StartSelectedServices", "audit.WaitReady", 1, "caller ctx, and the startup audit's own startupaudit.MaxDeadline (15m) / ProgressTimeout (<=30s)"},
	{"wait", "store/bolt/journal_entry.go", "(*Store).updateAndConfirm", "journal.WaitConfirmed", 1, "caller ctx; returns early when replication is unavailable"},
}

// TestWaitPointInventory fails when production code adds a wait point that is
// not listed above, or when a listed one no longer exists: a new wait states
// its bound here before it ships.
func TestWaitPointInventory(t *testing.T) {
	found := scanWaitPoints(t, "..")
	listed := map[waitPoint]int{}
	for _, p := range waitPoints {
		if strings.TrimSpace(p.bound) == "" {
			t.Errorf("wait point has no bound source: %+v", p)
		}
		key := waitPoint{kind: p.kind, file: p.file, function: p.function, call: p.call}
		if _, dup := listed[key]; dup {
			t.Errorf("wait point listed twice: %+v", key)
		}
		listed[key] = p.count
	}
	for key, count := range found {
		if listed[key] != count {
			t.Errorf("unlisted wait point (found %d, listed %d): {%q, %q, %q, %q}", count, listed[key], key.kind, key.file, key.function, key.call)
		}
	}
	for key, count := range listed {
		if _, ok := found[key]; !ok {
			t.Errorf("listed wait point no longer exists (listed %d): %+v", count, key)
		}
	}
}

func scanWaitPoints(t *testing.T, root string) map[waitPoint]int {
	t.Helper()
	found := map[waitPoint]int{}
	fset := token.NewFileSet()
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return err
		}
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			name := fn.Name.Name
			if fn.Recv != nil && len(fn.Recv.List) == 1 {
				name = "(" + render(fset, fn.Recv.List[0].Type) + ")." + name
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				var kind string
				switch {
				case sel.Sel.Name == "Wait" && len(call.Args) == 0:
					kind = "join"
				case sel.Sel.Name == "WorkQueueContext" || sel.Sel.Name == "AcquireWorkLane":
					kind = "queue"
				case strings.HasPrefix(sel.Sel.Name, "Wait"):
					kind = "wait"
				default:
					return true
				}
				found[waitPoint{kind: kind, file: filepath.ToSlash(rel), function: name, call: render(fset, sel.X) + "." + sel.Sel.Name}]++
				return true
			})
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return found
}

func render(fset *token.FileSet, node ast.Node) string {
	var b strings.Builder
	if err := printer.Fprint(&b, fset, node); err != nil {
		return ""
	}
	return b.String()
}
