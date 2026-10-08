//go:build linux

package app

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

// Opt-in to a real 64MiB page touch; requires cgroup v2 and scratch with known
// statfs inode capacity. No listener or real upstream request is opened here.
func TestResourcePressureRuntimeLinuxSmoke(t *testing.T) {
	if os.Getenv("HALRO_PRESSURE_RUNTIME_LINUX_SMOKE") != "1" {
		t.Skip("opt-in Linux runtime physical fixture")
	}
	cfg := testConfig(t)
	cfg.ResourcePressure = &config.ResourcePressure{RuntimeEnterBytes: 48 << 20, RuntimeExitBytes: 32 << 20, CgroupEnterBytes: 192 << 20, CgroupExitBytes: 160 << 20, HeadroomEnterBytes: 16 << 20, HeadroomExitBytes: 32 << 20, DiskEnterBytes: 64 << 10, DiskExitBytes: 128 << 10, InodesEnter: 16, InodesExit: 32, MaxAge: config.Duration(2 * time.Second), Cooldown: config.Duration(10 * time.Millisecond), SampleInterval: config.Duration(5 * time.Millisecond)}
	ramBudget := runtimeRAMBudgetFixture()
	cfg.ResourceBudget = &ramBudget
	if err := Initialize(cfg); err != nil {
		t.Fatal(err)
	}
	r, err := Open(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	waitRuntimePressure(t, r.resourcePressure, resourcebudget.Healthy)
	logical, err := r.resourcePressure.budget.TryAcquire(resourcebudget.Active, resourcebudget.Cost{RAM: ramBudget.RequestRAMBytes})
	if err != nil {
		t.Fatal(err)
	}
	defer logical.Release()
	before := r.resourcePressure.monitor.Snapshot()
	router := r.gatewayRouter()
	ready := httptest.NewRecorder()
	router.ServeHTTP(ready, httptest.NewRequest("GET", "/health/ready", nil))
	if ready.Code != http.StatusOK {
		t.Fatal(ready.Code, ready.Body.String())
	}
	held := make([]byte, 64<<20)
	for i := 0; i < len(held); i += 4096 {
		held[i] = 1
	}
	touchEnded := time.Now()
	waitRuntimePressure(t, r.resourcePressure, resourcebudget.Pressured)
	deadline := time.Now().Add(2 * time.Second)
	for r.resourcePressure.monitor.Snapshot().Sample.At.Before(touchEnded) && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	after := r.resourcePressure.monitor.Snapshot()
	if after.Sample.At.Before(touchEnded) || after.State != resourcebudget.Pressured {
		t.Fatal("missing post-touch physical sample", after)
	}
	if _, err = r.resourcePressure.budget.TryAcquire(resourcebudget.Active, resourcebudget.Cost{RAM: 1}); !errors.Is(err, resourcebudget.ErrPressure) {
		t.Fatal("physical pressure did not reach logical manager", err)
	}
	recovery, err := r.resourcePressure.budget.TryAcquireRecovery(resourcebudget.Cost{RAM: 4096})
	if err != nil {
		t.Fatal("independent recovery was blocked", err)
	}
	_ = recovery.Release()
	if s := r.resourcePressure.budget.Snapshot(); s.Classes[resourcebudget.Active].RAM != ramBudget.RequestRAMBytes {
		t.Fatal("pressure refunded live envelope", s)
	}
	_ = logical.Release()
	body := &countedBody{}
	refused := httptest.NewRecorder()
	router.ServeHTTP(refused, httptest.NewRequest("POST", "/v1/chat/completions", body))
	if refused.Code != http.StatusServiceUnavailable || body.reads.Load() != 0 || !strings.Contains(refused.Body.String(), "resource_pressure") {
		t.Fatal(refused.Code, refused.Body.String(), body.reads.Load())
	}
	ready = httptest.NewRecorder()
	router.ServeHTTP(ready, httptest.NewRequest("GET", "/health/ready", nil))
	if ready.Code != http.StatusServiceUnavailable {
		t.Fatal(ready.Code, ready.Body.String())
	}
	live := httptest.NewRecorder()
	router.ServeHTTP(live, httptest.NewRequest("GET", "/health/live", nil))
	if live.Code != http.StatusOK {
		t.Fatal(live.Code)
	}
	for _, entry := range []struct {
		Name     string
		Snapshot resourcebudget.PressureSnapshot
	}{{"before", before}, {"after_64MiB_touch", after}} {
		b, err := json.Marshal(entry)
		if err != nil {
			t.Fatal(err)
		}
		t.Log(string(b))
	}
	var metrics bytes.Buffer
	w := bufio.NewWriter(&metrics)
	r.writeResourcePressureMetrics(w)
	w.Flush()
	if !strings.Contains(metrics.String(), "state=\"pressured\"} 1") {
		t.Fatal(metrics.String())
	}
	if err = r.Close(); err != nil {
		t.Fatal(err)
	}
	if r.resourcePressure.monitor.Snapshot().Running {
		t.Fatal("Close left live sampler")
	}
	metrics.Reset()
	w.Reset(&metrics)
	r.writeResourcePressureMetrics(w)
	w.Flush()
	if strings.Contains(metrics.String(), "halro_resource_pressure_runtime_bytes") {
		t.Fatal("stopped sample republished as current", metrics.String())
	}
	runtime.KeepAlive(held)
}
