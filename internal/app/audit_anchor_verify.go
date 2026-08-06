package app

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/akz142857/Heimdall/internal/audit"
	"github.com/akz142857/Heimdall/internal/config"
	boltstore "github.com/akz142857/Heimdall/internal/store/bolt"
	"github.com/akz142857/Heimdall/internal/store/lock"
	"github.com/akz142857/Heimdall/internal/vault"
)

const (
	AnchorVerdictAgree     = "agree"
	AnchorVerdictDisagree  = "disagree"
	AnchorVerdictTruncated = "truncated"
)

// AnchorVerdict is the result of checking one previously emitted anchor
// (typically pulled off-host by a dead-man probe) against the current local
// audit chain. This is the detection ADR 0015 exists to provide: an anchor
// nobody controlling this host could have forged in step with the chain.
type AnchorVerdict struct {
	Sequence uint64 `json:"anchor_sequence"`
	Records  uint64 `json:"records"`
	Outcome  string `json:"outcome"`
}

// LoadAuditAnchorsFile reads a JSON-lines file of anchors — the format the
// dead-man probe's anchor sink writes (internal/deadman), one
// boltstore.AuditAnchor per line, appended as they are pulled off-host.
func LoadAuditAnchorsFile(path string) ([]boltstore.AuditAnchor, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open anchors file: %w", err)
	}
	defer file.Close()
	var anchors []boltstore.AuditAnchor
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var anchor boltstore.AuditAnchor
		if err := json.Unmarshal([]byte(line), &anchor); err != nil {
			return nil, fmt.Errorf("decode anchor line: %w", err)
		}
		anchors = append(anchors, anchor)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read anchors file: %w", err)
	}
	return anchors, nil
}

// VerifyAuditAnchors checks each of the given anchors against the local
// audit chain: the chain agrees at that sequence, disagrees (tampering), or
// is now shorter than the anchor claims (truncation). Like VerifyAudit, this
// runs offline against a stopped instance — it does not lock out concurrent
// appends, it requires the data directory's exclusive lock.
func VerifyAuditAnchors(ctx context.Context, cfg config.Config, anchors []boltstore.AuditAnchor) ([]AnchorVerdict, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	dataLock, err := lock.Acquire(cfg.Storage.DataDir)
	if err != nil {
		return nil, err
	}
	defer dataLock.Close()
	store, err := boltstore.Open(cfg.MetadataPath())
	if err != nil {
		return nil, err
	}
	defer store.Close()
	masterKey, err := unlockMasterKey(ctx, cfg, store)
	if err != nil {
		return nil, err
	}
	defer clear(masterKey)
	secretVault, err := vault.New(masterKey)
	if err != nil {
		return nil, err
	}
	defer secretVault.Close()
	if err := verifyVaultKeyCheck(store, secretVault); err != nil {
		return nil, err
	}
	auditKey, err := loadAuditHMACKey(store, secretVault, masterKey)
	if err != nil {
		return nil, err
	}
	defer clear(auditKey)
	log, err := audit.Open(cfg.AuditPath(), auditKey)
	if err != nil {
		return nil, fmt.Errorf("open local audit chain: %w", err)
	}
	defer log.Close()
	hashBySequence := make(map[uint64][32]byte)
	summary, err := log.Replay(func(record audit.Record) error {
		hashBySequence[record.Sequence] = record.Hash
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("replay local audit chain: %w", err)
	}
	verdicts := make([]AnchorVerdict, 0, len(anchors))
	for _, anchor := range anchors {
		verdict := AnchorVerdict{Sequence: anchor.Sequence, Records: anchor.Records}
		switch {
		case anchor.Records > summary.Records:
			verdict.Outcome = AnchorVerdictTruncated
		case hashBySequence[anchor.Records] == anchor.LastHash:
			verdict.Outcome = AnchorVerdictAgree
		default:
			verdict.Outcome = AnchorVerdictDisagree
		}
		verdicts = append(verdicts, verdict)
	}
	return verdicts, nil
}
