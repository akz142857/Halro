package app

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/akz142857/Halro/internal/vault"
)

const providerResourceReapInterval = time.Hour

func (r *Runtime) runProviderResourceMaintenance(ctx context.Context) {
	r.reclaimUnsealedProviderObjects(ctx)
	r.reapProviderResources(ctx)
	ticker := time.NewTicker(providerResourceReapInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.reapProviderResources(ctx)
		}
	}
}

func (r *Runtime) reapProviderResources(ctx context.Context) {
	expired, err := r.store.ExpiredProviderResources(ctx, time.Now().UTC())
	if err != nil {
		if !errors.Is(err, context.Canceled) {
			r.logger.Error("provider resource TTL reaper failed", "error", err)
		}
		return
	}
	for _, resource := range expired {
		if err := r.gatewayService.CleanupExpiredProviderResource(ctx, resource); err != nil {
			r.logger.Error("provider resource cleanup failed", "resource_id", resource.ID, "error", err)
		}
	}
}

// reclaimUnsealedProviderObjects drops the records whose object was written
// before provider objects were sealed.
//
// Those bytes cannot be served: the reader now expects an envelope, and there
// is no key that opens a file that was never sealed. Keeping the record would
// leave a resource that answers metadata queries and fails every read, and
// keeping the file would leave a plaintext prompt or model output on disk for
// as long as the install lives — which is the thing sealing exists to end. One
// summary line is logged rather than one per record: the count is the operator's
// business, the identifiers are not.
func (r *Runtime) reclaimUnsealedProviderObjects(ctx context.Context) {
	resources, err := r.store.ListProviderResources(ctx)
	if err != nil {
		if !errors.Is(err, context.Canceled) {
			r.logger.Error("provider object reclamation could not read the resource records", "error", err)
		}
		return
	}
	directory := filepath.Join(r.config.Storage.DataDir, "provider-objects")
	reclaimed := 0
	for _, resource := range resources {
		if resource.ObjectPath == "" || filepath.Base(resource.ObjectPath) != resource.ObjectPath {
			continue
		}
		path := filepath.Join(directory, resource.ObjectPath)
		header, err := readObjectHeader(path)
		if err != nil {
			if !errors.Is(err, os.ErrNotExist) {
				r.logger.Error("provider object could not be read", "resource_id", resource.ID, "error", err)
			}
			continue
		}
		if vault.SealedEnvelope(header) {
			continue
		}
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			r.logger.Error("unsealed provider object could not be removed", "resource_id", resource.ID, "error", err)
			continue
		}
		if err := r.store.DeleteProviderResource(ctx, resource.ProjectID, resource.ID); err != nil {
			r.logger.Error("unsealed provider object record could not be removed", "resource_id", resource.ID, "error", err)
			continue
		}
		reclaimed++
	}
	reclaimed += removeUnsealedObjectFiles(directory, r.logger)
	if reclaimed > 0 {
		r.logger.Warn("reclaimed provider objects written before sealing", "count", reclaimed)
	}
}

// removeUnsealedObjectFiles sweeps the directory itself, because walking the
// records only finds objects a record still names. A write that stored its
// bytes and then failed to store its record leaves a file nothing points at,
// and before sealing that file was a plaintext prompt or a plaintext result.
//
// Only unsealed files are removed. A sealed file is either in use or unreadable
// without its record, and either way removing it would race a write that is
// happening right now. Dot-prefixed entries are skipped for the same reason:
// that is the temporary name an in-flight write holds before its rename.
func removeUnsealedObjectFiles(directory string, logger *slog.Logger) int {
	entries, err := os.ReadDir(directory)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			logger.Error("provider object directory could not be listed", "error", err)
		}
		return 0
	}
	removed := 0
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || strings.HasPrefix(name, ".") {
			continue
		}
		path := filepath.Join(directory, name)
		header, err := readObjectHeader(path)
		if err != nil || vault.SealedEnvelope(header) {
			continue
		}
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			logger.Error("unsealed provider object could not be removed", "error", err)
			continue
		}
		removed++
	}
	return removed
}

// readObjectHeader reads only as much as SealedEnvelope inspects. A batch result
// can be large and nothing here needs its contents.
func readObjectHeader(path string) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	header := make([]byte, 6)
	n, err := io.ReadFull(file, header)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return nil, err
	}
	return header[:n], nil
}
