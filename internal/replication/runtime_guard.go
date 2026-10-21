package replication

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

const ClusterDirectoryName = "cluster"

// GuardStandaloneAccess refuses an operation that writes a data directory
// outside the replication order when that directory belongs to an HA member.
// Offline commands that write metadata, the Ledger or Audit (init, bootstrap,
// key and admin maintenance, ledger seal, usage rebuilds, route clears) only
// know the Standalone path; on a member their writes would never enter the
// ordering journal and the members would silently diverge. The on-disk cluster
// directory is an independent sentinel: removing the replication block must
// not turn durable member state into permission to write.
//
// This check is deliberately read-only and runs before the data-directory lock
// or Master Key unlock. A configured member without local state belongs to the
// explicit join/re-seed path; an unconfigured instance with any cluster
// directory belongs to cluster leave/recovery. Neither case may fall through to
// Standalone initialization.
func GuardStandaloneAccess(dataDir string, configured bool, operation string) error {
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
		return fmt.Errorf("refusing %s on an HA member: it would mutate member state at %s outside the replication order; member state changes through the running member or `halro cluster`", operation, clusterPath)
	case errors.Is(err, os.ErrNotExist):
		if configured {
			return fmt.Errorf("replication is configured but member state is absent at %s; refusing %s because it would mutate member state outside the replication order; join or re-seed must establish the member first", clusterPath, operation)
		}
		return nil
	default:
		return fmt.Errorf("inspect replication state before %s: %w", operation, err)
	}
}

// RequireMemberRuntime is the online role-aware counterpart to
// GuardStandaloneAccess. It authorizes only an explicitly configured member
// whose cluster directory and authenticated-state input already exist. It does
// not create either path: join/re-seed owns the first publication, while a
// missing replication block can never downgrade durable member state to
// Standalone.
func RequireMemberRuntime(dataDir string, configured bool, operation string) error {
	clusterPath := filepath.Join(dataDir, ClusterDirectoryName)
	if !configured {
		return GuardStandaloneAccess(dataDir, false, operation)
	}
	info, err := os.Lstat(clusterPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("replication is configured but member state is absent at %s; refusing %s; join or re-seed must establish the member first", clusterPath, operation)
		}
		return fmt.Errorf("inspect replication state before %s: %w", operation, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("refusing %s: replication state path %s is not a directory", operation, clusterPath)
	}
	statePath := filepath.Join(clusterPath, "state.json")
	stateInfo, err := os.Lstat(statePath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("replication is configured but authenticated member state is absent at %s; refusing %s; join or re-seed must establish the member first", statePath, operation)
		}
		return fmt.Errorf("inspect authenticated member state before %s: %w", operation, err)
	}
	if !stateInfo.Mode().IsRegular() {
		return fmt.Errorf("refusing %s: authenticated member state %s is not a regular file", operation, statePath)
	}
	return nil
}

// GuardMemberRead admits an offline command that appends nothing to any store
// — the verify commands and listings (HA design §12: "read-only" means no
// authoritative frame) — on a Standalone instance and on a seeded, configured
// member alike. It refuses what GuardStandaloneAccess refuses about the
// directory itself: member state without its configuration, or configuration
// without member state.
func GuardMemberRead(dataDir string, configured bool, operation string) error {
	if configured {
		return RequireMemberRuntime(dataDir, true, operation)
	}
	return GuardStandaloneAccess(dataDir, false, operation)
}
