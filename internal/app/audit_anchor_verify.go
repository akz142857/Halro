package app

import (
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
	// AnchorVerdictMissing marks a sequence the series skips over. Judging
	// only the anchors that are present lets whoever can edit the witness file
	// delete the inconvenient ones and get a clean report back.
	AnchorVerdictMissing = "missing"
	// AnchorVerdictMisordered marks a sequence that repeats or goes backwards,
	// which a single emitter cannot produce.
	AnchorVerdictMisordered = "misordered"
	// AnchorVerdictUnwitnessed marks the records appended since the newest
	// anchor. It is normal in small amounts and is the window a truncation
	// would aim for in large ones.
	AnchorVerdictUnwitnessed = "unwitnessed"
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
	payload, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("open anchors file: %w", err)
	}
	// A torn final line is an expected artifact, not corruption: the witness
	// appends one JSON object at a time and is a separate process that can be
	// killed mid-write. Refusing the whole file for it threw away every intact
	// anchor because of the one the crash interrupted — the witness silenced by
	// the same event that made it worth consulting. A short tail is dropped;
	// anything malformed with a line after it is not, because that is tampering
	// or real damage, and quietly skipping it is exactly how someone removes the
	// anchors that disagree.
	tornTail := len(payload) > 0 && payload[len(payload)-1] != '\n'
	lines := strings.Split(string(payload), "\n")
	var anchors []boltstore.AuditAnchor
	for index, raw := range lines {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		var anchor boltstore.AuditAnchor
		if err := json.Unmarshal([]byte(line), &anchor); err != nil {
			if tornTail && index == len(lines)-1 {
				break
			}
			return nil, fmt.Errorf("decode anchor line %d: %w", index+1, err)
		}
		anchors = append(anchors, anchor)
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
	expected := uint64(1)
	var highestRecords uint64
	for _, anchor := range anchors {
		verdict := AnchorVerdict{Sequence: anchor.Sequence, Records: anchor.Records}
		switch {
		case anchor.Records > summary.Records:
			verdict.Outcome = AnchorVerdictTruncated
		case anchor.Sequence < expected:
			// Sequences only ever go up. One that repeats or goes backwards
			// means the file was edited or two instances were merged into it,
			// and either way the anchors after it cannot be read as a series.
			verdict.Outcome = AnchorVerdictMisordered
		case hashBySequence[anchor.Records] == anchor.LastHash:
			verdict.Outcome = AnchorVerdictAgree
		default:
			verdict.Outcome = AnchorVerdictDisagree
		}
		if anchor.Sequence > expected {
			// Checking only the anchors present lets an attacker who can edit
			// the witness file delete the ones that disagree: every line left
			// agrees, and the report comes back clean. A gap is not proof of
			// tampering — the emitter's ring drops old anchors, and a witness
			// offline long enough will miss some — but it is the difference
			// between "these anchors agree" and "the record is complete", and
			// only the report can say which one the operator is looking at.
			verdicts = append(verdicts, AnchorVerdict{
				Sequence: expected, Outcome: AnchorVerdictMissing,
			})
		}
		if anchor.Sequence >= expected {
			expected = anchor.Sequence + 1
		}
		highestRecords = max(highestRecords, anchor.Records)
		verdicts = append(verdicts, verdict)
	}
	// An anchor covering the current chain length is what makes the whole
	// series meaningful. Without one, everything appended since the last
	// anchor is unwitnessed, which is exactly the window a truncation would
	// aim for.
	if summary.Records > highestRecords {
		verdicts = append(verdicts, AnchorVerdict{
			Records: summary.Records, Outcome: AnchorVerdictUnwitnessed,
		})
	}
	return verdicts, nil
}
