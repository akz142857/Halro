package gateway

import (
	"testing"

	"github.com/akz142857/Halro/internal/resourcebudget"
)

func TestGatewayCompletionWorkspaceUsesActualFixedSlotBound(t *testing.T) {
	f := newFixture(t, 1000000)
	defer f.close()
	resources, _, _, _ := bindActualCompletionAccounting(t, f)
	before := resources.Snapshot().Used
	for _, count := range []int{9, resourcebudget.MaxWorkspaceSlots, resourcebudget.MaxWorkspaceSlots + 1} {
		profile := make([]resourcebudget.WorkspaceRequirement, count)
		for i := range profile {
			profile[i] = resourcebudget.WorkspaceRequirement{Class: resourcebudget.IO, RAM: 4096}
		}
		options := ServiceOptions{ResourceBudget: resources, DeferredRAMBytes: 64 << 10, CompletionRAMBytes: int64(count) * 4096, CompletionWorkspace: profile}
		service, err := NewServiceWithOptions(f.service.auth, f.registry, f.service.accounting, options)
		if count <= resourcebudget.MaxWorkspaceSlots {
			if err != nil || service == nil || len(service.completionWorkspace) != count {
				t.Fatal("finite actual profile refused", count, err)
			}
		} else if err == nil || service != nil {
			t.Fatal("overflow profile accepted", count)
		}
		options.CompletionRAMBytes--
		if service, err := NewServiceWithOptions(f.service.auth, f.registry, f.service.accounting, options); err == nil || service != nil {
			t.Fatal("unfunded slot accepted", count)
		}
		if resources.Snapshot().Used != before {
			t.Fatal("factory manufactured or lost claims", count)
		}
	}
}
