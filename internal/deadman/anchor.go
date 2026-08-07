package deadman

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// PulledAnchor mirrors boltstore.AuditAnchor field-for-field (JSON tags, not
// a shared Go type — deadman is a separate binary and does not import the
// app's storage package). heimdall audit verify-anchor decodes the file
// anchorWriter produces using its own matching type.
type PulledAnchor struct {
	Sequence   uint64    `json:"sequence"`
	Records    uint64    `json:"records"`
	LastHash   [32]byte  `json:"last_hash"`
	Bytes      int64     `json:"bytes"`
	InstanceID string    `json:"instance_id"`
	ObservedAt time.Time `json:"observed_at"`
}

type anchorSink interface {
	append(PulledAnchor) error
}

type anchorWriter struct {
	mu   sync.Mutex
	path string
}

func (a *anchorWriter) append(anchor PulledAnchor) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := os.MkdirAll(filepath.Dir(a.path), 0o700); err != nil {
		return err
	}
	file, err := os.OpenFile(a.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	encoder := json.NewEncoder(file)
	if err := encoder.Encode(anchor); err != nil {
		file.Close()
		return fmt.Errorf("append pulled anchor: %w", err)
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return err
	}
	return file.Close()
}

// pullAnchors fetches new anchors from every heimdall-kind target that
// configures AnchorURL. It runs unlocked like probeTargets — the network
// calls here must not hold e.mu — and takes it only for the brief read/write
// of each target's LastAnchorSequence. Emission on the Heimdall side is
// fail-open (ADR 0015); the pull side matches that: an unreachable anchor
// endpoint is logged and skipped, never allowed to stall or fail a Tick that
// also carries the heartbeat.
func (e *Engine) pullAnchors(ctx context.Context) map[string]string {
	reasons := make(map[string]string)
	for _, target := range e.cfg.Targets {
		if target.Kind != "heimdall" || target.AnchorURL == "" || e.anchor == nil {
			continue
		}
		e.mu.Lock()
		since := e.state.Targets[target.ID].LastAnchorSequence
		e.mu.Unlock()
		anchors, err := fetchAnchors(ctx, e.cfg, target, e.targetClients[target.ID], since)
		if err != nil {
			e.logger.Warn("audit anchor pull failed", "target_id", target.ID, "error", err)
			reasons[target.ID] = "anchor_unreachable"
			continue
		}
		expected := since + 1
		reason := ""
		for _, anchor := range anchors {
			if anchor.Sequence <= since {
				// The endpoint was asked for anchors after `since` and answered
				// with ones at or before it. On a healthy instance that cannot
				// happen; after a restore from an older backup it does, because
				// the anchor sequence restarts. Skipping quietly is what made
				// the witness fall silent exactly when the log it witnesses had
				// been rolled back.
				reason = "anchor_sequence_rewound"
				continue
			}
			if anchor.Sequence != expected {
				// Anchors the ring dropped before this pull reached them. The
				// gap is permanent — nothing will re-serve those sequences — so
				// it has to be reported rather than closed over.
				reason = "anchor_sequence_gap"
			}
			if err := e.anchor.append(anchor); err != nil {
				e.logger.Warn("audit anchor persist failed", "target_id", target.ID, "sequence", anchor.Sequence, "error", err)
				reason = "anchor_persist_failed"
				break
			}
			since = anchor.Sequence
			expected = since + 1
			e.mu.Lock()
			state := e.state.Targets[target.ID]
			state.LastAnchorSequence = since
			e.state.Targets[target.ID] = state
			e.mu.Unlock()
		}
		if reason != "" {
			reasons[target.ID] = reason
		}
	}
	return reasons
}

func fetchAnchors(ctx context.Context, cfg Config, target TargetConfig, client *http.Client, since uint64) ([]PulledAnchor, error) {
	if client == nil {
		return nil, fmt.Errorf("no client configured for target %q", target.ID)
	}
	pullContext, cancel := context.WithTimeout(ctx, cfg.Timeout.Value())
	defer cancel()
	url := fmt.Sprintf("%s?since=%d", target.AnchorURL, since)
	response, err := request(pullContext, client, http.MethodGet, url, target.BearerTokenFile, nil)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
		return nil, fmt.Errorf("unexpected status %d", response.StatusCode)
	}
	var payload struct {
		Anchors []PulledAnchor `json:"anchors"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 4<<20)).Decode(&payload); err != nil {
		return nil, fmt.Errorf("decode anchor response: %w", err)
	}
	return payload.Anchors, nil
}
