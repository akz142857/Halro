package app

import (
	"cmp"
	"slices"
	"sync"

	"github.com/akz142857/Halro/internal/domain"
)

// capabilityMetrics holds the §13 observability counters for model capability
// selection. They are event counters — a scrape cannot reconstruct "how many
// times did the catalog fail to load" from current state, so they are recorded
// as they happen rather than derived at render time like the usage read-model.
//
// Every label is a bounded enum. Model IDs, provider IDs, deployment IDs and
// anything credential-derived are deliberately absent: §13 forbids them as
// Prometheus labels, and the specific object belongs in the audit trail, which
// already carries it.
type capabilityMetrics struct {
	mu                sync.Mutex
	catalogRefresh    map[catalogRefreshKey]uint64
	catalogDegraded   map[catalogDegradedKey]uint64
	drift             map[string]uint64
	deploymentTests   map[string]uint64
	revisionConflicts uint64
}

type catalogRefreshKey struct {
	ProviderType string
	Profile      string
	Status       string
}

type catalogDegradedKey struct {
	ProviderType string
	ErrorClass   string
}

// Drift reasons. §13 requires the two sources to be distinguishable: a catalog
// that moved is an upstream fact, a narrowed profile is something this binary
// did, and they call for different responses.
const (
	driftReasonCatalog = "catalog"
	driftReasonProfile = "profile"
)

// driftMetricReason collapses the review reasons onto the two sources §13 asks
// to be distinguishable. Everything the catalog did is one bucket; everything
// this binary did to its own profiles is the other.
func driftMetricReason(reviewReason string) string {
	if reviewReason == reviewReasonProfileNarrowed {
		return driftReasonProfile
	}
	return driftReasonCatalog
}

func newCapabilityMetrics() *capabilityMetrics {
	return &capabilityMetrics{
		catalogRefresh:  make(map[catalogRefreshKey]uint64),
		catalogDegraded: make(map[catalogDegradedKey]uint64),
		drift:           make(map[string]uint64),
		deploymentTests: make(map[string]uint64),
	}
}

func (m *capabilityMetrics) recordCatalogRefresh(providerType domain.ProviderType, profile domain.ProviderProfileID, ok bool) {
	status := "failure"
	if ok {
		status = "success"
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.catalogRefresh[catalogRefreshKey{string(providerType), string(profile), status}]++
}

func (m *capabilityMetrics) recordCatalogDegraded(providerType domain.ProviderType, errorClass string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.catalogDegraded[catalogDegradedKey{string(providerType), errorClass}]++
}

func (m *capabilityMetrics) recordDrift(reason string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.drift[reason]++
}

func (m *capabilityMetrics) recordDeploymentTest(ok bool) {
	status := "failure"
	if ok {
		status = "success"
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.deploymentTests[status]++
}

func (m *capabilityMetrics) recordRevisionConflict() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.revisionConflicts++
}

// capabilityMetricsSnapshot is a render-time copy with deterministic ordering,
// so repeated scrapes of unchanged state produce byte-identical output.
type capabilityMetricsSnapshot struct {
	CatalogRefresh    []catalogRefreshSample
	CatalogDegraded   []catalogDegradedSample
	Drift             map[string]uint64
	DeploymentTests   map[string]uint64
	RevisionConflicts uint64
}

type catalogRefreshSample struct {
	Key   catalogRefreshKey
	Count uint64
}

type catalogDegradedSample struct {
	Key   catalogDegradedKey
	Count uint64
}

func (m *capabilityMetrics) snapshot() capabilityMetricsSnapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	snapshot := capabilityMetricsSnapshot{
		Drift:             make(map[string]uint64, len(m.drift)),
		DeploymentTests:   make(map[string]uint64, len(m.deploymentTests)),
		RevisionConflicts: m.revisionConflicts,
	}
	for key, count := range m.catalogRefresh {
		snapshot.CatalogRefresh = append(snapshot.CatalogRefresh, catalogRefreshSample{key, count})
	}
	for key, count := range m.catalogDegraded {
		snapshot.CatalogDegraded = append(snapshot.CatalogDegraded, catalogDegradedSample{key, count})
	}
	for reason, count := range m.drift {
		snapshot.Drift[reason] = count
	}
	for status, count := range m.deploymentTests {
		snapshot.DeploymentTests[status] = count
	}
	slices.SortFunc(snapshot.CatalogRefresh, func(a, b catalogRefreshSample) int {
		return cmp.Or(
			cmp.Compare(a.Key.ProviderType, b.Key.ProviderType),
			cmp.Compare(a.Key.Profile, b.Key.Profile),
			cmp.Compare(a.Key.Status, b.Key.Status),
		)
	})
	slices.SortFunc(snapshot.CatalogDegraded, func(a, b catalogDegradedSample) int {
		return cmp.Or(
			cmp.Compare(a.Key.ProviderType, b.Key.ProviderType),
			cmp.Compare(a.Key.ErrorClass, b.Key.ErrorClass),
		)
	})
	return snapshot
}

// deploymentCapabilityGauges are the two §13 gauges. Unlike the counters these
// describe current state, so they are computed from the stored deployments at
// render time rather than tracked incrementally — a count that drifts from the
// records it claims to describe is worse than one that costs a read.
type deploymentCapabilityGauges struct {
	ByStatus         map[string]uint64
	OperatorDeclared uint64
}

// capabilityStatuses is fixed so every series is present from the first scrape.
// A gauge that only appears once it is non-zero cannot be alerted on with
// `== 0`, and "no drifted deployments" is exactly the condition worth asserting.
var capabilityStatuses = []string{"known", "partial", "unknown", "conflicting"}

func summariseDeploymentCapabilities(deployments []domain.Deployment) deploymentCapabilityGauges {
	gauges := deploymentCapabilityGauges{ByStatus: make(map[string]uint64, len(capabilityStatuses))}
	for _, status := range capabilityStatuses {
		gauges.ByStatus[status] = 0
	}
	for _, deployment := range deployments {
		if deployment.DeletedAt != nil {
			continue
		}
		snapshot := deployment.ModelCapabilitySnapshot
		if _, known := gauges.ByStatus[snapshot.Status]; known {
			gauges.ByStatus[snapshot.Status]++
		}
		if snapshot.Source == "operator_declared" {
			gauges.OperatorDeclared++
		}
	}
	return gauges
}
