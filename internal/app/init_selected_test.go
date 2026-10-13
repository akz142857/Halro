package app

import (
	"bytes"
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

	"github.com/akz142857/Halro/internal/budget"
	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/resourcebudget"
	"gopkg.in/yaml.v3"
)

func selectedInitFixture(t *testing.T) (config.Config, string, resourcebudget.SampleSource) {
	t.Helper()
	cfg := testConfig(t)
	limits, pressure := selectedRuntimeBudgetFixture(), runtimePressureFixture()
	pressure.MaxAge = config.Duration(time.Minute)
	cfg.ResourceBudget, cfg.ResourcePressure = &limits, &pressure
	raw, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(filepath.Dir(cfg.Storage.DataDir), "config.yaml")
	if err := os.WriteFile(configPath, raw, 0600); err != nil {
		t.Fatal(err)
	}
	return cfg, configPath, pressureSampleFunc(func(context.Context) resourcebudget.Sample { return runtimeLowSample() })
}

// openSelectedInitRuntime opens the initialized instance and requires the
// selected seven-root state, never the exchange-format Metadata database.
func openSelectedInitRuntime(t *testing.T, ctx context.Context, cfg config.Config, source resourcebudget.SampleSource) *Runtime {
	t.Helper()
	r, err := OpenWithOptions(ctx, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), OpenOptions{resourceSampleSource: source, AllowGeneratedSetupToken: true})
	if err != nil {
		t.Fatal("open initialized instance", err)
	}
	if r.selectedState == nil {
		r.Close()
		t.Fatal("initialized instance did not open the selected state")
	}
	return r
}

func TestSelectedInitializationProducesAReopenableSelectedInstance(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	cfg, configPath, source := selectedInitFixture(t)
	initialized, err := initializeSelected(ctx, cfg, configPath, source, nil)
	if err != nil || !initialized {
		t.Fatal("selected initialization", initialized, err)
	}
	archive, err := selectedInitArchivePath(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{archive, archive + ".pending"} {
		if _, err := os.Lstat(name); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("internal archive left beside the data directory", name, err)
		}
	}
	if again, err := initializeSelected(ctx, cfg, configPath, source, nil); err != nil || again {
		t.Fatal("rerun changed an activated instance", again, err)
	}

	r := openSelectedInitRuntime(t, ctx, cfg, source)
	project := domain.Project{ID: "prj_selected_init", Name: "selected init", Enabled: true}
	if _, err := r.store.PutProject(ctx, project, 0, nil); err != nil {
		r.Close()
		t.Fatal(err)
	}
	request, err := r.accounting.BeginRequestDetailed(ctx, project.ID, "key_selected_init", "req_selected_init", "chat")
	if err != nil {
		r.Close()
		t.Fatal(err)
	}
	attempt, err := r.accounting.ReserveAttempt(ctx, request, 20, 3)
	if err != nil {
		r.Close()
		t.Fatal(err)
	}
	if err := r.accounting.Settle(ctx, attempt, budget.Settlement{CommittedMicrosUSD: 7, Outcome: "success"}); err != nil {
		r.Close()
		t.Fatal(err)
	}
	if err := r.accounting.Finalize(ctx, request, "success"); err != nil {
		r.Close()
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}

	r = openSelectedInitRuntime(t, ctx, cfg, source)
	defer r.Close()
	if _, err := r.store.GetProject(ctx, project.ID); err != nil {
		t.Fatal("project lost across reopen", err)
	}
	if summary, found, err := r.usage.disk.ReadRequest(ctx, request.RequestID); err != nil || !found || summary.CostMicrosUSD != 7 {
		t.Fatal("finalized request lost across reopen", summary, found, err)
	}
}

// Every durable boundary of a first initialization: the exchange format, the
// internal archive, the namespace preparation hooks and the activation
// checkpoints, then the archive removal.
var selectedInitPhases = []string{"exchange-written", "archive-pending", "archive-published", "before-staging-create", "staging-created", "intent-created", "intent-synced", "staging-synced", "namespace-published", "namespace-synced", "sources-qualified", "roots-built", "bootstrap-pending-synced", "startup-caches-persisted", "common-seal-published", "bootstrap-activated", "activated"}

func TestSelectedInitializationChild(t *testing.T) {
	path := os.Getenv("HALRO_SELECTED_INIT_CHILD_CASE")
	if path == "" {
		t.Skip("subprocess only")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var wire struct {
		Config            config.Config
		ConfigPath, Phase string
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatal(err)
	}
	marker := os.Getenv("HALRO_SELECTED_INIT_CHILD_MARKER")
	source := pressureSampleFunc(func(context.Context) resourcebudget.Sample { return runtimeLowSample() })
	_, err = initializeSelected(context.Background(), wire.Config, wire.ConfigPath, source, func(phase string) error {
		if phase != wire.Phase {
			return nil
		}
		return selectedRotationChildPause(marker, phase)
	})
	t.Fatal("child escaped its kill point", err)
}

func TestSelectedInitializationSIGKILLAtEveryBoundaryResumes(t *testing.T) {
	for _, phase := range selectedInitPhases {
		t.Run(phase, func(t *testing.T) {
			stressPhase(t, phase, "archive-pending", "namespace-published", "roots-built", "bootstrap-pending-synced", "bootstrap-activated", "activated")
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
			defer cancel()
			cfg, configPath, source := selectedInitFixture(t)
			raw, err := json.Marshal(struct {
				Config            config.Config
				ConfigPath, Phase string
			}{cfg, configPath, phase})
			if err != nil {
				t.Fatal(err)
			}
			casePath, marker := filepath.Join(t.TempDir(), "case.json"), filepath.Join(t.TempDir(), "reached")
			if err := os.WriteFile(casePath, raw, 0o600); err != nil {
				t.Fatal(err)
			}
			child := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestSelectedInitializationChild$", "-test.count=1")
			child.Env = append(os.Environ(), "HALRO_SELECTED_INIT_CHILD_CASE="+casePath, "HALRO_SELECTED_INIT_CHILD_MARKER="+marker)
			var output bytes.Buffer
			child.Stdout, child.Stderr = &output, &output
			if err := child.Start(); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() { done <- child.Wait() }()
			ticker := time.NewTicker(time.Millisecond)
			defer ticker.Stop()
			for {
				if _, err := os.Lstat(marker); err == nil {
					break
				}
				select {
				case err := <-done:
					t.Fatal("child exited before its kill point", err, output.String())
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				case <-ticker.C:
				}
			}
			if err := child.Process.Signal(syscall.SIGKILL); err != nil {
				t.Fatal(err)
			}
			if err := <-done; err == nil {
				t.Fatal("SIGKILL did not end the child")
			}

			if initialized, err := initializeSelected(ctx, cfg, configPath, source, nil); err != nil {
				t.Fatal("rerun after SIGKILL", initialized, err)
			}
			archive, err := selectedInitArchivePath(cfg)
			if err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{archive, archive + ".pending"} {
				if _, err := os.Lstat(name); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("internal archive left beside the data directory", name, err)
				}
			}
			r := openSelectedInitRuntime(t, ctx, cfg, source)
			if err := r.Close(); err != nil {
				t.Fatal(err)
			}
			r = openSelectedInitRuntime(t, ctx, cfg, source)
			if err := r.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestSelectedInitializationRefusesAForeignDataDirectory(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cfg, configPath, source := selectedInitFixture(t)
	if err := os.MkdirAll(cfg.Storage.DataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	foreign := filepath.Join(cfg.Storage.DataDir, "unrelated.txt")
	if err := os.WriteFile(foreign, []byte("not Halro"), 0o600); err != nil {
		t.Fatal(err)
	}
	if initialized, err := initializeSelected(ctx, cfg, configPath, source, nil); err == nil || initialized {
		t.Fatal("initialized over an unrelated file", initialized, err)
	}
	entries, err := os.ReadDir(cfg.Storage.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.Name() != "unrelated.txt" && entry.Name() != ".halro.lock" {
			t.Fatal("refusal wrote into the data directory", entry.Name())
		}
	}
	archive, err := selectedInitArchivePath(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(archive); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("refusal created an internal archive", err)
	}
	if _, err := os.Lstat(cfg.Storage.MasterKey.File); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("refusal created a master key", err)
	}
}
