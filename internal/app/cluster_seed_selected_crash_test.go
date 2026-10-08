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
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/replication"
	"github.com/akz142857/Halro/internal/store/lock"
)

type selectedSeedCrashRequest struct {
	Config                        config.Config
	Stage, Manifest, Phase, Ready string
}

func TestSelectedSeedInstallerSIGKILLKeepsUnknownStageAndFreshReplicaQualification(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	fixture, owners, _ := prepareHASelectedTransportFixture(t, ctx)
	for _, f := range owners {
		if e := errors.Join(f.Close(), f.owners.Components.Guard.Close()); e != nil {
			t.Fatal(e)
		}
	}
	source := fixture.configs[0]
	manifestPath := filepath.Join(t.TempDir(), "seed.json")
	manifest, e := CreateSeedManifest(selectedSeedCommandTestContext(t, ctx, source), source, "halro-1", manifestPath, "admin", []byte("correct horse battery staple"), "")
	if e != nil {
		t.Fatal(e)
	}
	for _, phase := range []string{"source_preserved", "replica_seal_durable", "bootstrap_bound", "namespace_published"} {
		t.Run(phase, func(t *testing.T) {
			cfg := fixture.configs[1]
			cfg.Storage.DataDir = filepath.Join(t.TempDir(), "replica")
			stage := cfg.Storage.DataDir + ".stage"
			copyTestTree(t, source.Storage.DataDir, stage)
			request := selectedSeedCrashRequest{cfg, stage, manifestPath, phase, filepath.Join(t.TempDir(), "ready")}
			raw, e := json.Marshal(request)
			if e != nil {
				t.Fatal(e)
			}
			requestPath := filepath.Join(t.TempDir(), "request.json")
			if e = os.WriteFile(requestPath, raw, 0600); e != nil {
				t.Fatal(e)
			}
			cmd := exec.Command(os.Args[0], "-test.run=^TestSelectedSeedInstallerSIGKILLHelper$", "-test.count=1")
			cmd.Env = append(os.Environ(), "HALRO_SELECTED_SEED_CRASH_REQUEST="+requestPath)
			var output bytes.Buffer
			cmd.Stdout = &output
			cmd.Stderr = &output
			if e = cmd.Start(); e != nil {
				t.Fatal(e)
			}
			done := make(chan error, 1)
			go func() { done <- cmd.Wait() }()
			joined := false
			defer func() {
				if !joined {
					_ = cmd.Process.Kill()
					<-done
				}
			}()
			until := time.NewTimer(30 * time.Second)
			defer until.Stop()
			tick := time.NewTicker(10 * time.Millisecond)
			defer tick.Stop()
			ready := false
			for !ready {
				if _, e = os.Stat(request.Ready); e == nil {
					ready = true
					break
				}
				select {
				case e := <-done:
					joined = true
					t.Fatalf("child exited before %s: %v %s", phase, e, output.String())
				case <-until.C:
					t.Fatalf("child did not reach %s: %s", phase, output.String())
				case <-tick.C:
				}
			}
			if e = cmd.Process.Kill(); e != nil {
				t.Fatal(e)
			}
			e = <-done
			joined = true
			if e == nil {
				t.Fatal("child was not killed")
			}
			if phase != "namespace_published" {
				if _, e = os.Lstat(cfg.Storage.DataDir); !errors.Is(e, os.ErrNotExist) {
					t.Fatalf("interrupted stage published target: %v", e)
				}
				if _, e = os.Lstat(stage); e != nil {
					t.Fatal("unknown stage discarded", e)
				}
				before, e := os.ReadFile(filepath.Join(stage, "active.seal"))
				if e != nil {
					t.Fatal(e)
				}
				if _, e = InstallSeedSnapshot(selectedSeedCommandTestContext(t, ctx, cfg), cfg, stage, manifestPath); e == nil {
					t.Fatal("interrupted changed stage reused as approved source")
				}
				after, e := os.ReadFile(filepath.Join(stage, "active.seal"))
				if e != nil || !bytes.Equal(before, after) {
					t.Fatal("rejected retry changed unknown candidate", e)
				}
				// The operator may recopy the independently approved source. The original
				// interrupted stage is retained; a fresh sibling is independently hashed.
				stage += ".fresh"
				copyTestTree(t, source.Storage.DataDir, stage)
				if _, e = InstallSeedSnapshot(selectedSeedCommandTestContext(t, ctx, cfg), cfg, stage, manifestPath); e != nil {
					t.Fatal("fresh qualified recopy", e)
				}
			} else if _, e = os.Lstat(stage); !errors.Is(e, os.ErrNotExist) {
				t.Fatalf("published namespace still has stage: %v", e)
			}
			g, e := lock.Acquire(cfg.Storage.DataDir)
			if e != nil {
				t.Fatal(e)
			}
			defer g.Close()
			f, e := openHASelectedRuntimeFoundation(ctx, cfg, fixture.keys.Master, g, functionalHAMetadataManager(t, cfg), slog.New(slog.NewTextHandler(io.Discard, nil)))
			if e != nil {
				t.Fatal("fresh target actual permission", e)
			}
			if e = f.prepareSelectedServices(ctx); e != nil {
				t.Fatal(e)
			}
			if f.reopened.Seal.RootCount != 7 || f.reopened.Seal.OrderingIndex != manifest.Index || f.replication.publisher.Snapshot().Role != replication.RoleReplica {
				t.Fatal("fresh target lost true seven-root/member cut")
			}
			if _, e = f.metadataStore.GetAdminUser(ctx, "admin"); e != nil {
				t.Fatal(e)
			}
			if e = f.Close(); e != nil {
				t.Fatal("actual target owner join", e)
			}
		})
	}
}

func TestSelectedSeedInstallerSIGKILLHelper(t *testing.T) {
	requestPath := os.Getenv("HALRO_SELECTED_SEED_CRASH_REQUEST")
	if requestPath == "" {
		t.Skip("actual SIGKILL child")
	}
	raw, e := os.ReadFile(requestPath)
	if e != nil {
		t.Fatal(e)
	}
	var request selectedSeedCrashRequest
	if e = json.Unmarshal(raw, &request); e != nil {
		t.Fatal(e)
	}
	ctx := selectedSeedCommandTestContext(t, context.Background(), request.Config)
	ctx = context.WithValue(ctx, selectedSeedFaultKey{}, func(phase string) error {
		if phase != request.Phase {
			return nil
		}
		if e := os.WriteFile(request.Ready, []byte(phase), 0600); e != nil {
			return e
		}
		for {
			time.Sleep(time.Hour)
		}
	})
	if _, e = InstallSeedSnapshot(ctx, request.Config, request.Stage, request.Manifest); e != nil {
		t.Fatal(e)
	}
	t.Fatal("selected seed installer did not interrupt")
}
