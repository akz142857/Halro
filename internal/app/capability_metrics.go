package app

import (
	"cmp"
	"slices"
	"sync"
	"time"

	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/modelcatalog"
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
	mu                 sync.Mutex
	catalogRefresh     map[catalogRefreshKey]uint64
	catalogDegraded    map[catalogDegradedKey]uint64
	drift              map[string]uint64
	deploymentTests    map[string]uint64
	revisionConflicts  uint64
	resolutions        map[resolutionMetricKey]uint64
	detections         map[detectionMetricKey]uint64
	probes             map[probeMetricKey]uint64
	detectionInflight  map[string]int64
	detectionCache     map[string]uint64
	detectionCalls     map[string]uint64
	detectionDuration  map[detectionMetricKey]detectionDurationMetric
	signedCatalog      map[signedCatalogMetricKey]uint64
	signedCatalogState modelcatalog.ManagerStatus
}

type detectionMetricKey struct{ ProviderType, Status, Source string }
type probeMetricKey struct{ ProviderType, Capability, Status string }
type resolutionMetricKey struct{ ProviderType, TargetKind, Status, Source string }
type signedCatalogMetricKey struct{ Status, ErrorClass string }
type detectionDurationMetric struct {
	Count   uint64
	Sum     float64
	Buckets [6]uint64
}

var detectionDurationBounds = [6]float64{1, 5, 15, 30, 60, 90}

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
		resolutions:     make(map[resolutionMetricKey]uint64),
		detections:      make(map[detectionMetricKey]uint64), probes: make(map[probeMetricKey]uint64),
		detectionInflight: make(map[string]int64), detectionCache: make(map[string]uint64),
		detectionCalls: make(map[string]uint64), detectionDuration: make(map[detectionMetricKey]detectionDurationMetric),
		signedCatalog: make(map[signedCatalogMetricKey]uint64),
	}
}

func (m *capabilityMetrics) recordSignedCatalog(event modelcatalog.RefreshEvent) {
	status := "failure"
	if event.Outcome == "success" {
		status = "success"
	}
	errorClass := boundedCatalogErrorClass(event.ErrorClass)
	m.mu.Lock()
	defer m.mu.Unlock()
	m.signedCatalog[signedCatalogMetricKey{status, errorClass}]++
	m.signedCatalogState = event.Status
}

func (m *capabilityMetrics) setSignedCatalogState(status modelcatalog.ManagerStatus) {
	m.mu.Lock()
	m.signedCatalogState = status
	m.mu.Unlock()
}

func boundedCatalogErrorClass(value string) string {
	switch value {
	case "", "download", "signature", "schema", "expired", "pinned_revision", "size_limit", "rollback", "validation", "persistence", "activation":
		if value == "" {
			return "none"
		}
		return value
	default:
		return "unknown"
	}
}

func (m *capabilityMetrics) recordResolution(providerType domain.ProviderType, targetKind domain.DeploymentTargetKind, status domain.ResolutionState, source domain.ClaimSource) {
	key := resolutionMetricKey{
		ProviderType: boundedResolutionProviderType(providerType),
		TargetKind:   boundedResolutionTargetKind(targetKind),
		Status:       boundedResolutionStatus(status),
		Source:       boundedResolutionSource(source),
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.resolutions[key]++
}

func boundedResolutionProviderType(value domain.ProviderType) string {
	if _, known := domain.DefaultProviderProfile(value); known {
		return string(value)
	}
	return "unknown"
}

func boundedResolutionTargetKind(value domain.DeploymentTargetKind) string {
	switch value {
	case domain.TargetModelID, domain.TargetAzureDeployment, domain.TargetBedrockFoundationModel,
		domain.TargetBedrockInferenceProfile, domain.TargetBedrockProvisionedThroughput, domain.TargetCustomEndpointModel:
		return string(value)
	default:
		return "unknown"
	}
}

func boundedResolutionStatus(value domain.ResolutionState) string {
	switch value {
	case domain.ResolutionResolved, domain.ResolutionUnknown, domain.ResolutionConflicting,
		domain.ResolutionNoVariant, domain.ResolutionCoveredElsewhere:
		return string(value)
	default:
		return "unknown"
	}
}

func boundedResolutionSource(value domain.ClaimSource) string {
	switch value {
	case domain.ClaimSourceBuiltinCatalog, domain.ClaimSourceProviderMetadata, domain.ClaimSourceSignedCatalog,
		domain.ClaimSourceVerifiedProbe, domain.ClaimSourceOperatorDeclared:
		return string(value)
	case "":
		return "none"
	default:
		return "unknown"
	}
}

func (m *capabilityMetrics) recordDetectionStart(providerType string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.detectionInflight[providerType]++
}
func (m *capabilityMetrics) recordDetectionFinish(providerType, status, source string, results map[string]domain.CapabilityProbeResult, calls int, duration time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := detectionMetricKey{providerType, status, source}
	m.detections[key]++
	if m.detectionInflight[providerType] > 0 {
		m.detectionInflight[providerType]--
	}
	for capability, result := range results {
		m.probes[probeMetricKey{providerType, capability, string(result.Status)}]++
	}
	m.detectionCalls[providerType] += uint64(calls)
	seconds := duration.Seconds()
	histogram := m.detectionDuration[key]
	histogram.Count++
	histogram.Sum += seconds
	for index, bound := range detectionDurationBounds {
		if seconds <= bound {
			histogram.Buckets[index]++
		}
	}
	m.detectionDuration[key] = histogram
}
func (m *capabilityMetrics) recordDetectionCache(status string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.detectionCache[status]++
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
	CatalogRefresh     []catalogRefreshSample
	CatalogDegraded    []catalogDegradedSample
	Drift              map[string]uint64
	DeploymentTests    map[string]uint64
	RevisionConflicts  uint64
	Resolutions        map[resolutionMetricKey]uint64
	Detections         map[detectionMetricKey]uint64
	Probes             map[probeMetricKey]uint64
	DetectionInflight  map[string]int64
	DetectionCache     map[string]uint64
	DetectionCalls     map[string]uint64
	DetectionDuration  map[detectionMetricKey]detectionDurationMetric
	SignedCatalog      map[signedCatalogMetricKey]uint64
	SignedCatalogState modelcatalog.ManagerStatus
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
		Resolutions:       make(map[resolutionMetricKey]uint64, len(m.resolutions)),
		Detections:        make(map[detectionMetricKey]uint64, len(m.detections)), Probes: make(map[probeMetricKey]uint64, len(m.probes)),
		DetectionInflight: make(map[string]int64, len(m.detectionInflight)), DetectionCache: make(map[string]uint64, len(m.detectionCache)),
		DetectionCalls: make(map[string]uint64, len(m.detectionCalls)), DetectionDuration: make(map[detectionMetricKey]detectionDurationMetric, len(m.detectionDuration)),
		SignedCatalog: make(map[signedCatalogMetricKey]uint64, len(m.signedCatalog)), SignedCatalogState: m.signedCatalogState,
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
	for key, count := range m.detections {
		snapshot.Detections[key] = count
	}
	for key, count := range m.probes {
		snapshot.Probes[key] = count
	}
	for key, count := range m.resolutions {
		snapshot.Resolutions[key] = count
	}
	for key, count := range m.detectionInflight {
		snapshot.DetectionInflight[key] = count
	}
	for key, count := range m.detectionCache {
		snapshot.DetectionCache[key] = count
	}
	for key, count := range m.detectionCalls {
		snapshot.DetectionCalls[key] = count
	}
	for key, value := range m.detectionDuration {
		snapshot.DetectionDuration[key] = value
	}
	for key, value := range m.signedCatalog {
		snapshot.SignedCatalog[key] = value
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
	ByStatus map[string]uint64
	// Unrecognised holds deployments whose snapshot status is none of the four.
	// It should stay zero; it exists so that it can be seen if it does not,
	// rather than the record vanishing from the totals.
	Unrecognised     uint64
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
		} else {
			gauges.Unrecognised++
		}
		if snapshot.Source == "operator_declared" {
			gauges.OperatorDeclared++
		}
	}
	return gauges
}
