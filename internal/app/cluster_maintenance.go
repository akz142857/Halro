package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"crypto/sha256"

	"github.com/akz142857/Halro/internal/bearercred"
	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/durable"
	"github.com/akz142857/Halro/internal/replication"
	"github.com/akz142857/Halro/internal/vault"
)

const maintenanceFileName = "maintenance"

// ErrMaintenanceRequested tells the serving command that a live Replica has
// drained and closed its data directory specifically to enter the maintenance
// listener loop. It is an internal lifecycle transition, not a process error.
var ErrMaintenanceRequested = errors.New("Replica maintenance requested")

func maintenancePath(cfg config.Config) string {
	return filepath.Join(cfg.ClusterDirectoryPath(), maintenanceFileName)
}

func memberMaintenanceRequested(cfg config.Config) (bool, error) {
	info, err := os.Lstat(maintenancePath(cfg))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return false, errors.New("cluster maintenance sentinel must be a private regular file")
	}
	return true, nil
}

// SetMemberMaintenance publishes or removes the node-local maintenance
// sentinel. It deliberately does not take the data lock: the command is run
// inside a live Replica Pod, whose serve process observes the sentinel on its
// next restart and stays alive without acquiring the lock.
func SetMemberMaintenance(ctx context.Context, cfg config.Config, enabled bool) error {
	state, err := ClusterStatus(ctx, cfg)
	if err != nil {
		return err
	}
	if state.Role != replication.RoleReplica {
		return errors.New("maintenance mode may be changed only on a Replica")
	}
	path := maintenancePath(cfg)
	if enabled {
		if _, err := os.Lstat(path); err == nil {
			return nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		temporary, err := os.CreateTemp(cfg.ClusterDirectoryPath(), ".maintenance-*")
		if err != nil {
			return err
		}
		temporaryPath := temporary.Name()
		defer os.Remove(temporaryPath)
		if err := temporary.Chmod(0o600); err != nil {
			_ = temporary.Close()
			return err
		}
		if _, err := temporary.WriteString("maintenance\n"); err != nil {
			_ = temporary.Close()
			return err
		}
		if err := temporary.Sync(); err != nil {
			_ = temporary.Close()
			return err
		}
		if err := temporary.Close(); err != nil {
			return err
		}
		if err := os.Rename(temporaryPath, path); err != nil {
			return err
		}
		return durable.SyncDirectory(cfg.ClusterDirectoryPath())
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return durable.SyncDirectory(cfg.ClusterDirectoryPath())
}

// WaitForMemberMaintenance keeps liveness endpoints up while intentionally
// leaving the data-directory lock available to an offline Replica backup. It
// returns when the sentinel is removed, after which the caller opens the real
// runtime in the same process.
func WaitForMemberMaintenance(ctx context.Context, cfg config.Config, logger *slog.Logger) (bool, error) {
	if cfg.Replication == nil {
		return false, nil
	}
	requested, err := memberMaintenanceRequested(cfg)
	if err != nil {
		return false, err
	}
	if !requested {
		return false, nil
	}
	runtime := &Runtime{config: cfg, logger: logger}
	if cfg.Metrics.Enabled && cfg.Metrics.RequireAuth {
		if cfg.Metrics.CredentialFile != "" {
			runtime.metricsAuthorizer, err = bearercred.NewAuthorizer(cfg.Metrics.CredentialFile)
			if err != nil {
				return false, fmt.Errorf("load maintenance metrics credentials: %w", err)
			}
		} else {
			masterKey, unlockErr := unlockMemberMasterKey(ctx, cfg)
			if unlockErr != nil {
				return false, unlockErr
			}
			token, deriveErr := vault.DeriveMetricsBearerToken(masterKey)
			clear(masterKey)
			if deriveErr != nil {
				return false, deriveErr
			}
			runtime.metricsTokenHash = sha256.Sum256(token)
			clear(token)
		}
	}
	if err := runtime.openTLSMaterial(); err != nil {
		return false, err
	}
	healthHandler, metricsHandler := maintenanceHandlers(runtime, cfg)
	type boundServer struct {
		server   *http.Server
		listener net.Listener
	}
	addresses := []struct{ name, address string }{{"gateway", cfg.Server.GatewayListen}, {"admin", cfg.Server.AdminListen}}
	if cfg.Metrics.Enabled {
		addresses = append(addresses, struct{ name, address string }{"metrics", cfg.Server.MetricsListen})
	}
	bound := make([]boundServer, 0, len(addresses))
	for _, item := range addresses {
		handler := http.Handler(healthHandler)
		if item.name == "metrics" {
			handler = metricsHandler
		}
		server := runtime.server(item.name, item.address, handler)
		listener, listenErr := net.Listen("tcp", item.address)
		if listenErr != nil {
			for _, existing := range bound {
				_ = existing.listener.Close()
			}
			return false, fmt.Errorf("bind maintenance listener %s: %w", item.address, listenErr)
		}
		bound = append(bound, boundServer{server: server, listener: listener})
	}
	errs := make(chan error, len(bound))
	for _, item := range bound {
		item := item
		go func() {
			var serveErr error
			if item.server.TLSConfig != nil {
				serveErr = item.server.ServeTLS(item.listener, "", "")
			} else {
				serveErr = item.server.Serve(item.listener)
			}
			if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
				errs <- serveErr
			}
		}()
	}
	logger.Info("Replica maintenance mode active; data directory lock is available for offline work")
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	var waitErr error
	waiting := true
	for waiting {
		select {
		case <-ctx.Done():
			waitErr = ctx.Err()
			waiting = false
		case waitErr = <-errs:
			waiting = false
		case <-ticker.C:
			_, statErr := os.Lstat(maintenancePath(cfg))
			if errors.Is(statErr, os.ErrNotExist) {
				waiting = false
			} else if statErr != nil {
				waitErr = statErr
				waiting = false
			}
		}
	}
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cfg.Server.ShutdownTimeout.Value())
	defer cancel()
	var shutdownErrs []error
	for _, item := range bound {
		shutdownErrs = append(shutdownErrs, item.server.Shutdown(shutdownCtx))
	}
	return true, errors.Join(waitErr, errors.Join(shutdownErrs...))
}

func maintenanceHandlers(runtime *Runtime, cfg config.Config) (http.Handler, http.Handler) {
	healthHandler := http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/health/live" {
			writeJSON(writer, http.StatusOK, map[string]string{"status": "live", "mode": "maintenance"})
			return
		}
		writer.Header().Set("Retry-After", "1")
		writeJSON(writer, http.StatusServiceUnavailable, map[string]string{"status": "not_ready", "mode": "maintenance"})
	})
	metricsHandler := http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/metrics" {
			healthHandler.ServeHTTP(writer, request)
			return
		}
		if cfg.Metrics.RequireAuth && !runtime.authorizeMetrics(request) {
			writer.Header().Set("WWW-Authenticate", `Bearer realm="metrics"`)
			writeJSON(writer, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}
		writer.Header().Set("Content-Type", "text/plain; version=0.0.4")
		_, _ = writer.Write([]byte("# HELP halro_cluster_maintenance Whether this member intentionally released its data lock for maintenance.\n# TYPE halro_cluster_maintenance gauge\nhalro_cluster_maintenance 1\n"))
	})
	return healthHandler, metricsHandler
}
