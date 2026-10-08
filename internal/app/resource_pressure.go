package app

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"

	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

type resourcePressureRuntime struct {
	monitor                   *resourcebudget.Monitor
	rejected                  atomic.Uint64
	budget                    *resourcebudget.Manager
	requestRAM, completionRAM int64
	completionDisk            int64
	completionWorkspace       []resourcebudget.WorkspaceRequirement
	deferredRAM, captureRAM   int64
}

// A shutdown audit is a new fact, not a previously accepted request's
// completion. The selected physical generation can still publish and join
// accepted facts under pressure, but cannot spend their original grants on
// a new audit or its Metadata checkpoint. Check the actual admission owner;
// a failure after starting an audit still propagates through Close normally.
func (r *Runtime) canAppendShutdownAudit() bool {
	if r.selectedState == nil || r.config.ResourceBudget == nil || !r.config.ResourceBudget.PhysicalTerminalReserve {
		return true
	}
	return r.resourcePressure != nil && r.resourcePressure.budget != nil &&
		r.resourcePressure.budget.Snapshot().Pressure == resourcebudget.Healthy
}

// checkOperationDiskHeadroom extends the configured data/temp filesystem
// sample with the filesystem that will receive a one-shot output. It is a
// preflight only; it cannot reserve blocks or prevent later ENOSPC.
func checkOperationDiskHeadroom(ctx context.Context, cfg config.Config, outputDir string) error {
	if cfg.ResourcePressure == nil {
		return nil
	}
	if !filepath.IsAbs(outputDir) {
		return fmt.Errorf("operation output directory must be absolute")
	}
	var err error
	outputDir, err = existingOperationDirectory(outputDir)
	if err != nil {
		return err
	}
	// Restore and initialization intentionally target an absent directory.
	// Sample its existing ancestor before creating any staged output; the
	// online sampler still requires the actual live data directory.
	dataDir, err := existingOperationDirectory(cfg.Storage.DataDir)
	if err != nil {
		return err
	}
	sampler, err := resourcebudget.NewSampler(dataDir)
	if err != nil {
		return fmt.Errorf("create disk headroom sampler: %w", err)
	}
	available, inodes, err := sampler.DiskHeadroom(ctx, outputDir)
	if err != nil {
		return fmt.Errorf("sample operation output filesystem headroom: %w", err)
	}
	if operationDiskHeadroomBelowStopLine(available, inodes, cfg.ResourcePressure) {
		return fmt.Errorf("operation output filesystem is at or below the configured disk/inode stop line")
	}
	return ctx.Err()
}

func existingOperationDirectory(outputDir string) (string, error) {
	if !filepath.IsAbs(outputDir) {
		return "", fmt.Errorf("operation directory must be absolute")
	}
	outputDir = filepath.Clean(outputDir)
	for {
		info, err := os.Stat(outputDir)
		if err == nil {
			if !info.IsDir() {
				return "", fmt.Errorf("operation output parent is not a directory")
			}
			break
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("inspect operation output directory: %w", err)
		}
		parent := filepath.Dir(outputDir)
		if parent == outputDir {
			return "", fmt.Errorf("operation output directory has no existing ancestor")
		}
		outputDir = parent
	}
	return outputDir, nil
}

func operationDiskHeadroomBelowStopLine(available, inodes int64, policy *config.ResourcePressure) bool {
	return policy != nil && (available < 0 || inodes < 0 || available <= policy.DiskEnterBytes || inodes <= policy.InodesEnter)
}

func newResourcePressure(cfg config.Config) (*resourcePressureRuntime, error) {
	return newResourcePressureWithSource(cfg, nil)
}

func newResourcePressureWithSource(cfg config.Config, source resourcebudget.SampleSource) (*resourcePressureRuntime, error) {
	if cfg.ResourcePressure == nil {
		if cfg.ResourceBudget != nil {
			return nil, fmt.Errorf("resource_budget requires resource_pressure")
		}
		return nil, nil
	}
	if err := cfg.ResourcePressure.Validate(); err != nil {
		return nil, fmt.Errorf("resource_pressure: %w", err)
	}
	if source == nil {
		sampler, err := resourcebudget.NewSampler(cfg.Storage.DataDir)
		if err != nil {
			return nil, fmt.Errorf("resource_pressure sampler: %w", err)
		}
		source = sampler
	}
	monitor, err := resourcebudget.NewMonitor(source, cfg.ResourcePressure.Policy(), cfg.ResourcePressure.SampleInterval.Value())
	if err != nil {
		return nil, err
	}
	p := &resourcePressureRuntime{monitor: monitor}
	if cfg.ResourceBudget != nil {
		if err := cfg.ResourceBudget.Validate(cfg.ResourcePressure, cfg.Server.MaxRequestBytes); err != nil {
			return nil, fmt.Errorf("resource_budget: %w", err)
		}
		if err := cfg.ResourceBudget.ValidateCapture(cfg.Gateway.FailureCapture); err != nil {
			return nil, fmt.Errorf("resource_budget: %w", err)
		}
		if cfg.Replication != nil {
			if err := cfg.ResourceBudget.ValidateReplicationCandidates(); err != nil {
				return nil, fmt.Errorf("resource_budget: %w", err)
			}
			if err := cfg.ResourceBudget.ValidateTerminalReplication(cfg.Gateway.MaxTotalAttempts); err != nil {
				return nil, fmt.Errorf("resource_budget: %w", err)
			}
		}
		p.budget, err = resourcebudget.New(cfg.ResourceBudget.Limits(cfg.ResourcePressure.MaxAge.Value()))
		if err != nil {
			return nil, err
		}
		if err = monitor.AttachManager(p.budget); err != nil {
			return nil, err
		}
		p.requestRAM, p.deferredRAM = cfg.ResourceBudget.RequestRAMBytes, cfg.ResourceBudget.DeferredRAMBytes
		p.completionRAM = cfg.ResourceBudget.CompletionRAMBytes
		p.completionDisk = cfg.ResourceBudget.CompletionDiskBytes
		p.captureRAM = cfg.ResourceBudget.CaptureRAMBytes
	}
	return p, nil
}

func (p *resourcePressureRuntime) logicalBudget() *resourcebudget.Manager {
	if p == nil {
		return nil
	}
	return p.budget
}
func (p *resourcePressureRuntime) deferredEnvelope() int64 {
	if p == nil {
		return 0
	}
	return p.deferredRAM
}

func (p *resourcePressureRuntime) completionRAMEnvelope() int64 {
	if p == nil {
		return 0
	}
	return p.completionRAM
}

func (p *resourcePressureRuntime) completionProfile() []resourcebudget.WorkspaceRequirement {
	if p == nil {
		return nil
	}
	return append([]resourcebudget.WorkspaceRequirement(nil), p.completionWorkspace...)
}

// The class ceiling funds all concurrent owners. A selected request reserves
// only its finite terminal profile, leaving already-paid recovery/checkpoint
// work funded independently. Disk retains the configured request envelope.
func (p *resourcePressureRuntime) setSelectedCompletionProfile(profile []resourcebudget.WorkspaceRequirement) error {
	if p == nil || p.budget == nil || len(profile) == 0 {
		return errors.New("selected completion profile requires its shared resource owner")
	}
	var ram int64
	for _, slot := range profile {
		if (slot.Class != resourcebudget.IO && slot.Class != resourcebudget.Background) || slot.RAM <= 0 || slot.RAM > p.completionRAM-ram {
			return errors.New("selected completion profile exceeds completion RAM ceiling")
		}
		ram += slot.RAM
	}
	p.completionWorkspace = append([]resourcebudget.WorkspaceRequirement(nil), profile...)
	p.completionRAM = ram
	return nil
}

func (p *resourcePressureRuntime) completionDiskEnvelope() int64 {
	if p == nil {
		return 0
	}
	return p.completionDisk
}

func (p *resourcePressureRuntime) captureEnvelope() int64 {
	if p == nil {
		return 0
	}
	return p.captureRAM
}

func (r *Runtime) runResourcePressure(ctx context.Context, pressure *resourcePressureRuntime) {
	if r.selectedState != nil {
		// The selected generation owns this same running physical monitor until
		// common publication and actual resource retirement have finished.
		<-ctx.Done()
		return
	}
	// Capture the immutable subsystem before starting the worker in Open.
	err := pressure.monitor.Run(ctx, func(s resourcebudget.PressureSnapshot) {
		if s.State == resourcebudget.Healthy {
			r.logger.Info("resource pressure admission recovered", "samples", s.Samples)
		} else {
			r.logger.Warn("resource pressure admission closed", "state", pressureName(s.State), "signals", pressureIssueNames(s.Issues), "samples", s.Samples)
		}
	})
	if err != nil && ctx.Err() == nil {
		r.logger.Error("resource pressure monitor stopped", "error", err)
	}
}

// Installed only on routes that accept new work. Existing-work cancellation,
// queries, outcome reporting and close do not re-enter this admission boundary.
func (r *Runtime) requireResourcePressure(protocol staleErrorProtocol) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			p := r.resourcePressure
			if p == nil {
				next.ServeHTTP(w, req)
				return
			}
			code := "resource_pressure"
			if p.monitor.Snapshot().State == resourcebudget.Healthy {
				if p.budget == nil {
					next.ServeHTTP(w, req)
					return
				}
				active := resourcebudget.Cost{RAM: p.requestRAM}
				completion := resourcebudget.Cost{RAM: p.completionRAM, Disk: p.completionDisk}
				if len(p.completionWorkspace) != 0 {
					work, err := p.budget.TryAcquireCompletableWork(resourcebudget.Active, active, completion, p.completionWorkspace)
					if err == nil {
						defer work.Active.Release()
						defer work.Close()
						next.ServeHTTP(w, req.WithContext(work.Bind(req.Context())))
						return
					}
					writeResourceCapacityError(w, protocol, "resource_budget")
					return
				}
				if completion != (resourcebudget.Cost{}) {
					work, err := p.budget.TryAcquireWork(resourcebudget.Active, active, completion)
					if err == nil {
						// Keep both claims through body decode, provider IO, synchronous
						// finalization/capture encoding and the final downstream write.
						// Async copies and persistent domain state need separate owners.
						defer work.Active.Release()
						defer work.Completion.Release()
						next.ServeHTTP(w, req)
						return
					}
					code = "resource_budget"
					writeResourceCapacityError(w, protocol, code)
					return
				}
				permit, err := p.budget.TryAcquire(resourcebudget.Active, active)
				if err == nil {
					// Keep the envelope through body decode, provider IO, synchronous
					// finalization/capture encoding and the final downstream write.
					// Async copies and persistent domain state need separate owners.
					defer permit.Release()
					next.ServeHTTP(w, req)
					return
				}
				code = "resource_budget"
				writeResourceCapacityError(w, protocol, code)
				return
			}
			for old := p.rejected.Load(); old < math.MaxUint64; old = p.rejected.Load() {
				if p.rejected.CompareAndSwap(old, old+1) {
					break
				}
			}
			writeResourceCapacityError(w, protocol, code)
		})
	}
}

func writeResourceCapacityError(w http.ResponseWriter, protocol staleErrorProtocol, code string) {
	w.Header().Set("Cache-Control", "no-store")
	const message = "resource capacity is unavailable; retry new work later"
	if protocol == staleErrorAnthropic {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"type": "error", "error": map[string]string{"type": "overloaded_error", "message": code + ": " + message}})
		return
	}
	writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": map[string]any{"type": "service_unavailable", "code": code, "message": message, "param": nil}})
}

func pressureName(p resourcebudget.Pressure) string {
	switch p {
	case resourcebudget.Healthy:
		return "healthy"
	case resourcebudget.Pressured:
		return "pressured"
	default:
		return "unknown"
	}
}

func (r *Runtime) writeResourcePressureMetrics(w *bufio.Writer) {
	r.writeResourceBudgetMetrics(w)
	enabled := 0
	if r.resourcePressure != nil {
		enabled = 1
	}
	metricHeader(w, "halro_resource_pressure_enabled", "gauge", "Whether calibrated physical admission is configured; not a complete logical resource budget.")
	fmt.Fprintf(w, "halro_resource_pressure_enabled %d\n", enabled)
	if enabled == 0 {
		return
	}
	s := r.resourcePressure.monitor.Snapshot()
	metricHeader(w, "halro_resource_pressure_state", "gauge", "Fresh physical admission state; unknown and pressured refuse new work.")
	for _, state := range []resourcebudget.Pressure{resourcebudget.Unknown, resourcebudget.Healthy, resourcebudget.Pressured} {
		v := 0
		if s.State == state {
			v = 1
		}
		fmt.Fprintf(w, "halro_resource_pressure_state{state=%q} %d\n", pressureName(state), v)
	}
	metricHeader(w, "halro_resource_pressure_samples_total", "counter", "Published bounded physical samples in this runtime.")
	fmt.Fprintf(w, "halro_resource_pressure_samples_total %d\n", s.Samples)
	metricHeader(w, "halro_resource_pressure_rejections_total", "counter", "New Gateway work rejected before reading its body.")
	fmt.Fprintf(w, "halro_resource_pressure_rejections_total %d\n", r.resourcePressure.rejected.Load())
	metricHeader(w, "halro_resource_pressure_signal_issue", "gauge", "Fixed reasons for unavailable, stale, high or cooling physical signals.")
	for _, reason := range pressureReasons {
		v := 0
		if s.Issues&reason.flag != 0 {
			v = 1
		}
		fmt.Fprintf(w, "halro_resource_pressure_signal_issue{reason=%q} %d\n", reason.name, v)
	}
	// Missing measurements are omitted, never rendered as zero usage.
	if s.Samples == 0 || s.Issues&(resourcebudget.SignalInvalid|resourcebudget.SampleStale|resourcebudget.ClockInvalid|resourcebudget.SampleCancelled|resourcebudget.MonitorInactive) != 0 {
		return
	}
	if s.Sample.Issues&resourcebudget.RuntimeUnavailable == 0 && s.Sample.RuntimeUsed >= 0 {
		metricHeader(w, "halro_resource_pressure_runtime_bytes", "gauge", "Go total minus released bytes; not RSS.")
		fmt.Fprintf(w, "halro_resource_pressure_runtime_bytes %d\n", s.Sample.RuntimeUsed)
	}
	if s.Sample.Issues&resourcebudget.CgroupUnavailable == 0 && s.Sample.GroupCount > 0 && s.Sample.GroupCount <= resourcebudget.MaxCgroupDepth && s.Sample.Groups[0].Current >= 0 {
		metricHeader(w, "halro_resource_pressure_cgroup_bytes", "gauge", "Latest visible leaf cgroup current bytes, including descendants.")
		fmt.Fprintf(w, "halro_resource_pressure_cgroup_bytes %d\n", s.Sample.Groups[0].Current)
	}
	if s.Sample.Issues&resourcebudget.DiskUnavailable == 0 && s.Sample.DiskAvailable >= 0 && s.Sample.InodesAvailable >= 0 {
		metricHeader(w, "halro_resource_pressure_disk_available_bytes", "gauge", "Lowest available bytes across the data-directory and system temporary-directory filesystems.")
		fmt.Fprintf(w, "halro_resource_pressure_disk_available_bytes %d\n", s.Sample.DiskAvailable)
		metricHeader(w, "halro_resource_pressure_inodes_available", "gauge", "Lowest known available inodes across the data-directory and system temporary-directory filesystems.")
		fmt.Fprintf(w, "halro_resource_pressure_inodes_available %d\n", s.Sample.InodesAvailable)
	}
}

// Labels are a fixed vocabulary, never filesystem paths, IDs or error strings.
var pressureReasons = [...]struct {
	flag resourcebudget.SignalIssue
	name string
}{
	{resourcebudget.RuntimeUnavailable, "runtime_unavailable"}, {resourcebudget.CgroupUnavailable, "cgroup_unavailable"},
	{resourcebudget.DiskUnavailable, "disk_unavailable"}, {resourcebudget.SampleCancelled, "sample_cancelled"},
	{resourcebudget.SignalInvalid, "signal_invalid"}, {resourcebudget.SampleStale, "sample_stale"},
	{resourcebudget.ClockInvalid, "clock_invalid"}, {resourcebudget.SampleRepeated, "sample_repeated"},
	{resourcebudget.SampleGap, "sample_gap"}, {resourcebudget.SourceChanged, "source_changed"},
	{resourcebudget.RuntimeHigh, "runtime_high"}, {resourcebudget.CgroupHigh, "cgroup_high"},
	{resourcebudget.CgroupHeadroomLow, "cgroup_headroom_low"}, {resourcebudget.DiskLow, "disk_low"},
	{resourcebudget.InodesLow, "inodes_low"}, {resourcebudget.Cooling, "cooling"}, {resourcebudget.MonitorInactive, "monitor_inactive"},
}

func pressureIssueNames(issues resourcebudget.SignalIssue) []string {
	out := make([]string, 0, len(pressureReasons))
	for _, reason := range pressureReasons {
		if issues&reason.flag != 0 {
			out = append(out, reason.name)
		}
	}
	return out
}
