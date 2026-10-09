package config

import (
	"encoding/json"
	"math"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/resourcebudget"
)

func TestHAPhysicalWorkMinimumMatchesSimultaneousAdmission(t *testing.T) {
	b := ResourceBudget{MemoryStateRequired: true, PhysicalTerminalReserve: true, RequestRAMBytes: 16 << 20, PricingIORAMBytes: 32 << 20}
	normal, completion, claims, err := b.HAPhysicalWorkMinimum()
	if err != nil {
		t.Fatal(err)
	}
	b.RecoveryClaims = 1
	b.RecoveryReserveRAMBytes = resourcebudget.ClaimRAMBytes
	b.TotalRAMBytes = normal + b.RecoveryReserveRAMBytes
	b.CompletionRAMBytes = completion
	b.MaxClaims = claims + 1
	if err := b.validateHAPhysicalWorkMinimum(); err != nil {
		t.Fatal(err)
	}
	l := resourcebudget.Limits{Total: resourcebudget.Cost{RAM: b.TotalRAMBytes}, RecoveryReserve: resourcebudget.Cost{RAM: b.RecoveryReserveRAMBytes}, MaxClaims: b.MaxClaims, RecoveryClaims: 1, MaxOwners: 8, MaxPressureAge: time.Minute}
	l.Classes[resourcebudget.Active].RAM = b.RequestRAMBytes
	l.Classes[resourcebudget.IO].RAM = normal - completion - b.RequestRAMBytes
	l.Classes[resourcebudget.Background].RAM = normal - completion - b.RequestRAMBytes
	l.Classes[resourcebudget.Completion].RAM = completion
	l.Classes[resourcebudget.Control].RAM = int64(l.MaxClaims) * resourcebudget.ClaimRAMBytes
	m, err := resourcebudget.New(l)
	if err != nil {
		t.Fatal(err)
	}
	// Unit-level admission exercise only; production qualification uses its OS sampler.
	if err := m.SetPressure(resourcebudget.Healthy); err != nil {
		t.Fatal(err)
	}
	shared, err := m.InstallSharedCompletableWorkspace(b.HAPhysicalSharedProfiles())
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := shared.Close(); err != nil {
			t.Error(err)
		}
	}()
	var works []*resourcebudget.CompletableWork
	defer func() {
		for _, w := range works {
			if err := w.Close(); err != nil {
				t.Error(err)
			}
			_ = w.Active.Release()
		}
	}()
	for _, p := range b.HAPhysicalWorkProfiles() {
		var ram int64
		for _, r := range p.Requirements {
			ram += r.RAM
		}
		w, err := m.TryAcquireCompletableWork(p.Class, p.Active, resourcebudget.Cost{RAM: ram}, p.Requirements)
		if err != nil {
			t.Fatal(p.Name, err)
		}
		works = append(works, w)
	}
	pricing, err := m.TryAcquire(resourcebudget.IO, resourcebudget.Cost{RAM: b.PricingIORAMBytes})
	if err != nil {
		t.Fatal(err)
	}
	defer pricing.Release()
	var residents []resourcebudget.Permit
	defer func() {
		for _, p := range residents {
			if err := p.Release(); err != nil {
				t.Error(err)
			}
		}
	}()
	for _, r := range b.HAPhysicalResidentWorkRequirements() {
		p, err := m.TryAcquire(r.Class, resourcebudget.Cost{RAM: r.RAM})
		if err != nil {
			t.Fatal("selected resident owner", err)
		}
		residents = append(residents, p)
	}
	got := m.Snapshot()
	if got.Normal.RAM != normal || got.NormalClaims != claims || got.Classes[resourcebudget.Completion].RAM != completion {
		t.Fatal(normal, completion, claims, got)
	}
	report, err := json.Marshal(struct {
		Profiles           []HAPhysicalWorkProfile
		Normal, Completion int64
		Claims             int
		Residents          []resourcebudget.WorkspaceRequirement
		Shared             []resourcebudget.SharedWorkProfile
	}{b.HAPhysicalWorkProfiles(), normal, completion, claims, b.HAPhysicalResidentWorkRequirements(), b.HAPhysicalSharedProfiles()})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("HA_PHYSICAL_VECTOR %s", report)
	for name, change := range map[string]func(*ResourceBudget){
		"normal-one-byte-short":     func(v *ResourceBudget) { v.TotalRAMBytes-- },
		"completion-one-byte-short": func(v *ResourceBudget) { v.CompletionRAMBytes-- },
		"claim-one-short":           func(v *ResourceBudget) { v.MaxClaims-- },
		"request-overflow":          func(v *ResourceBudget) { v.RequestRAMBytes = math.MaxInt64 },
		"zero-active-request":       func(v *ResourceBudget) { v.RequestRAMBytes = 0 },
		"negative-reserve":          func(v *ResourceBudget) { v.RecoveryReserveRAMBytes = -1 },
	} {
		t.Run(name, func(t *testing.T) {
			bad := b
			change(&bad)
			if err := bad.validateHAPhysicalWorkMinimum(); err == nil {
				t.Fatal("undersized HA simultaneous vector accepted")
			}
		})
	}
	if err := pricing.Release(); err != nil {
		t.Fatal(err)
	}
	for _, w := range works {
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
		if err := w.Active.Release(); err != nil {
			t.Fatal(err)
		}
	}
	for _, p := range residents {
		if err := p.Release(); err != nil {
			t.Fatal(err)
		}
	}
	residents = nil
	works = nil
	if err := shared.Close(); err != nil {
		t.Fatal(err)
	}
	if got := m.Snapshot(); got.Normal.RAM != 0 || got.NormalClaims != 0 || got.Owners != 0 {
		t.Fatal("actual owners not retired", got)
	}
}

// Even with request completion on the shared lane, the work vector (which now
// counts the Audit/Governance native batch encoders the Linux run found
// holding RAM) exceeds the original 2 GiB / 1387 MiB Recovery envelope. The
// rejection must come from the vector itself, not from unrelated fields.
func TestHAPhysicalWorkMinimumRejectsOriginal2GiBAndPreservesOptOut(t *testing.T) {
	b := ResourceBudget{MemoryStateRequired: true, PhysicalTerminalReserve: true, TotalRAMBytes: 2 << 30, RecoveryReserveRAMBytes: 1387 << 20, RequestRAMBytes: 16 << 20, PricingIORAMBytes: 32 << 20, CompletionRAMBytes: 2 << 30, MaxClaims: 1024, RecoveryClaims: 32}
	normal, _, _, err := b.HAPhysicalWorkMinimum()
	if err != nil {
		t.Fatal(err)
	}
	if err := b.validateHAPhysicalWorkMinimum(); err == nil || normal <= b.TotalRAMBytes-b.RecoveryReserveRAMBytes {
		t.Fatal("original 2 GiB work vector accepted", normal, err)
	}
	t.Logf("original 2 GiB normal deficit %d bytes", normal-(b.TotalRAMBytes-b.RecoveryReserveRAMBytes))
	exact := b
	exact.TotalRAMBytes = b.RecoveryReserveRAMBytes + normal
	if err := exact.validateHAPhysicalWorkMinimum(); err != nil {
		t.Fatal("exact vector rejected", err)
	}
	exact.TotalRAMBytes--
	if err := exact.validateHAPhysicalWorkMinimum(); err == nil {
		t.Fatal("one byte short accepted")
	}
	b.PhysicalTerminalReserve = false
	if err := b.validateHAPhysicalWorkMinimum(); err != nil {
		t.Fatal("legacy opt-out changed", err)
	}
}

// The original 2 GiB HA production budget. Its Recovery floor is the candidate
// formula; its ceiling is the physical work vector's normal minimum. Charging
// the offline backup workspace beside three sessions once left no value between
// them; without it the window is about 128 MiB wide.
func TestOriginal2GiBHABudgetLeavesARecoveryWindow(t *testing.T) {
	b := ResourceBudget{MemoryStateRequired: true, PhysicalTerminalReserve: true,
		PersistentInventoryDiskBytes: 512 << 20, LedgerAppendDiskBytes: 64 << 20, AccountingRecoveryRAMBytes: 128 << 20,
		LedgerSegmentIndexDiskBytes: 64 << 20, LedgerProjectionDiskBytes: 256 << 20, UsageProjectionDiskBytes: 256 << 20,
		UsageArchiveDiskBytes: 64 << 20, UsageArchiveReadRAMBytes: 8 << 20, GovernanceProjectionDiskBytes: 64 << 20,
		GovernanceLogDiskBytes: 64 << 20, AuditLogDiskBytes: 64 << 20, MetadataStoreDiskBytes: 128 << 20,
		LegacyMetadataFileDiskBytes: 64 << 20, MetadataJournalDiskBytes: 576 << 20, OrderingJournalDiskBytes: 1 << 20,
		BackupWorkspaceDiskBytes: 128 << 20, TotalRAMBytes: 2 << 30, TotalDiskBytes: 32 << 30, TotalUnconfirmedBytes: 64 << 20,
		ConfigurationDiskBytes: 128 << 20, ActiveDiskBytes: 128 << 20, CacheDiskBytes: 128 << 20, IODiskBytes: 1 << 30,
		IOUnconfirmedBytes: 32 << 20, BackgroundDiskBytes: 16 << 30, CompletionDiskBytes: 128 << 20, CompletionUnconfirmedBytes: 32 << 20,
		RecoveryDiskBytes: 8 << 30, RecoveryReserveDiskBytes: 8 << 30, RecoveryUnconfirmedBytes: 16 << 20, RecoveryReserveUnconfirmedBytes: 16 << 20,
		UnconfirmedMaxEntries: 64, ConfigurationRAMBytes: 256 << 20, ActiveRAMBytes: 256 << 20, CacheRAMBytes: 256 << 20,
		IORAMBytes: 512 << 20, BackgroundRAMBytes: 1 << 30, CompletionRAMBytes: 1 << 30, ControlRAMBytes: 160 << 10,
		MaxClaims: 160, RecoveryClaims: 32, MaxOwners: 16, RequestRAMBytes: 16 << 20, DeferredRAMBytes: 16 << 20,
		ReplicationCandidateRAMBytes: 100689312, ReplicationQueueRAMBytes: 67126528, ReplicationReadRAMBytes: 100689312,
		ReplicationReaderRAMBytes: 384 << 10, AuditLogRAMBytes: 2 << 20, GovernanceLogRAMBytes: 2 << 20,
		PricingLifetimeRAMBytes: 1 << 20, PricingIORAMBytes: 32 << 20}
	at := func(r int64) error {
		c := b
		c.RecoveryRAMBytes, c.RecoveryReserveRAMBytes = r, r
		return c.ValidateReplicationCandidates()
	}
	const floor, ceiling = 1319742656, 1386 << 20
	if err := at(floor); err != nil {
		t.Fatal("candidate floor rejected", err)
	}
	if err := at(1259 << 20); err != nil {
		t.Fatal("1259 MiB rejected", err)
	}
	if err := at(ceiling); err != nil {
		t.Fatal("work vector ceiling rejected", err)
	}
	if at(floor-1) == nil || at(1387<<20) == nil {
		t.Fatal("Recovery window bounds moved")
	}
}
