package app

import (
	"bufio"
	"fmt"

	"github.com/akz142857/Halro/internal/resourcebudget"
)

func (r *Runtime) writeResourceBudgetMetrics(w *bufio.Writer) {
	b := r.resourcePressure.logicalBudget()
	enabled := 0
	if b != nil {
		enabled = 1
	}
	metricHeader(w, "halro_resource_budget_enabled", "gauge", "Whether shared logical resource envelopes are configured; filesystem blocks are not reserved.")
	fmt.Fprintf(w, "halro_resource_budget_enabled %d\n", enabled)
	if b == nil {
		return
	}
	s := b.Snapshot()
	metricHeader(w, "halro_resource_budget_ram_bytes", "gauge", "Logical RAM currently charged, including claim metadata; not actual heap or RSS.")
	fmt.Fprintf(w, "halro_resource_budget_ram_bytes %d\n", s.Used.RAM)
	metricHeader(w, "halro_resource_budget_disk_bytes", "gauge", "Logical persistent bytes currently charged; not filesystem free space or preallocation.")
	fmt.Fprintf(w, "halro_resource_budget_disk_bytes %d\n", s.Used.Disk)
	metricHeader(w, "halro_resource_budget_class_ram_bytes", "gauge", "Charged logical RAM by fixed resource class.")
	metricHeader(w, "halro_resource_budget_class_disk_bytes", "gauge", "Charged logical disk bytes by fixed resource class; does not preallocate blocks.")
	metricHeader(w, "halro_resource_budget_acquired_total", "counter", "Successful claims by fixed class.")
	metricHeader(w, "halro_resource_budget_rejections_total", "counter", "Rejected logical claim attempts by fixed class, including unavailable pressure.")
	for i, name := range [...]string{"configuration", "active", "cache", "io", "background", "completion", "recovery", "control"} {
		fmt.Fprintf(w, "halro_resource_budget_class_ram_bytes{class=%q} %d\n", name, s.Classes[i].RAM)
		fmt.Fprintf(w, "halro_resource_budget_class_disk_bytes{class=%q} %d\n", name, s.Classes[i].Disk)
		fmt.Fprintf(w, "halro_resource_budget_acquired_total{class=%q} %d\n", name, s.Acquired[i])
		fmt.Fprintf(w, "halro_resource_budget_rejections_total{class=%q} %d\n", name, s.Rejected[i])
	}
	metricHeader(w, "halro_resource_budget_claims", "gauge", "Live claims by normal or independent recovery pool.")
	fmt.Fprintf(w, "halro_resource_budget_claims{pool=\"normal\"} %d\n", s.NormalClaims)
	fmt.Fprintf(w, "halro_resource_budget_claims{pool=\"recovery\"} %d\n", s.RecoveryClaims)
	metricHeader(w, "halro_resource_budget_owners", "gauge", "Live logical permit owners.")
	fmt.Fprintf(w, "halro_resource_budget_owners %d\n", s.Owners)
	metricHeader(w, "halro_resource_budget_pressure_state", "gauge", "Manager pressure lease state, including monitor cancellation and stop.")
	for _, state := range []resourcebudget.Pressure{resourcebudget.Unknown, resourcebudget.Healthy, resourcebudget.Pressured} {
		v := 0
		if s.Pressure == state {
			v = 1
		}
		fmt.Fprintf(w, "halro_resource_budget_pressure_state{state=%q} %d\n", pressureName(state), v)
	}
}
