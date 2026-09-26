package replication

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

const ClusterDirectoryName = "cluster"

// GuardUnavailableRuntime keeps an HA member from being opened through the
// Standalone path while the role-aware runtime is still being built. The
// on-disk directory is an independent sentinel: removing the replication block
// must not turn durable member state into permission to write outside the
// cluster order.
//
// This check is deliberately read-only and runs before the data-directory lock
// or Master Key unlock. A configured member without local state belongs to the
// explicit join/re-seed path; an unconfigured instance with any cluster
// directory belongs to cluster leave/recovery. Neither case may fall through to
// Standalone initialization.
func GuardUnavailableRuntime(dataDir string, configured bool, operation string) error {
	clusterPath := filepath.Join(dataDir, ClusterDirectoryName)
	info, err := os.Lstat(clusterPath)
	switch {
	case err == nil:
		if !info.IsDir() {
			return fmt.Errorf("refusing %s: replication state path %s is not a directory", operation, clusterPath)
		}
		if !configured {
			return fmt.Errorf("refusing %s: HA member state exists at %s but replication is absent; removing configuration cannot downgrade a member to Standalone", operation, clusterPath)
		}
		return fmt.Errorf("replication is configured and member state exists at %s, but this build has no replication runtime; refusing %s because it would mutate member state outside the replication order", clusterPath, operation)
	case errors.Is(err, os.ErrNotExist):
		if configured {
			return fmt.Errorf("replication is configured but member state is absent at %s; refusing %s because it would mutate member state outside the replication order; join or re-seed must establish the member first", clusterPath, operation)
		}
		return nil
	default:
		return fmt.Errorf("inspect replication state before %s: %w", operation, err)
	}
}
