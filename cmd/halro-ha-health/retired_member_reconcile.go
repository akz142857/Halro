package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/akz142857/Halro/internal/app"
	halroconfig "github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/replication"
)

// retiredMemberReconcileReport describes only the authenticated old journal
// tail imported into a stopped collector. It does not authorize or represent
// acceptance of a replacement member journal.
type retiredMemberReconcileReport struct {
	Status                string `json:"status"`
	NodeID                string `json:"node_id"`
	Incarnation           string `json:"incarnation"`
	JournalID             string `json:"journal_id"`
	FromSequence          uint64 `json:"from_sequence"`
	ImportedEvents        uint64 `json:"imported_events"`
	CommittedSequence     uint64 `json:"committed_sequence"`
	CommittedDigest       string `json:"committed_digest"`
	MemberInventorySHA256 string `json:"member_inventory_sha256"`
}

func reconcileRetiredMemberSnapshot(ctx context.Context, archivePath, environment, cluster string, members []string,
	cfg halroconfig.Config, snapshotDir string) (retiredMemberReconcileReport, error) {
	if cfg.Replication == nil || cfg.Replication.ClusterID != cluster || !slices.Contains(members, cfg.Replication.NodeID) {
		return retiredMemberReconcileReport{}, errors.New("retired member configuration is outside the expected collector inventory")
	}
	archive, err := openDurableArchive(archivePath, environment, cluster, members)
	if err != nil {
		return retiredMemberReconcileReport{}, err
	}
	defer archive.Close()
	node := cfg.Replication.NodeID
	chains := archive.doc.Chains[node]
	if len(chains) == 0 {
		return retiredMemberReconcileReport{}, errors.New("collector has no retired member chain to reconcile")
	}
	chain := chains[len(chains)-1]
	start := chain.Cursor
	result := retiredMemberReconcileReport{NodeID: node, Incarnation: chain.Incarnation,
		JournalID: start.JournalID, FromSequence: start.Sequence}
	var inventory string
	for {
		member, page, err := app.ReadMemberTransitionSnapshotPage(ctx, cfg, snapshotDir,
			replication.DurableTransitionCursor{JournalID: start.JournalID, Sequence: start.Sequence, Digest: start.Digest})
		if err != nil {
			return retiredMemberReconcileReport{}, fmt.Errorf("authenticate retired member tail: %w", err)
		}
		if member.ClusterID != cluster || member.NodeID != node || member.Incarnation != chain.Incarnation ||
			member.JournalID != chain.Cursor.JournalID || member.BaselineDigest != chain.Cursor.BaselineDigest ||
			member.BaselineKind != chain.BaselineKind || member.Committed < start.Sequence ||
			page.CommittedSequence != member.Committed || page.HeadDigest != member.CommittedDigest ||
			inventory != "" && inventory != member.InventorySHA256 {
			return retiredMemberReconcileReport{}, errors.New("retired member identity, head or frozen inventory changed")
		}
		inventory = member.InventorySHA256
		encoded, err := json.Marshal(page)
		if err != nil {
			return retiredMemberReconcileReport{}, err
		}
		var verified durableTransitionPage
		if err := decodeExactJSON(encoded, &verified); err != nil {
			return retiredMemberReconcileReport{}, fmt.Errorf("decode authenticated retired member page: %w", err)
		}
		if verified.NextCursor.JournalID != start.JournalID || len(verified.Events) == 0 && verified.HasMore ||
			verified.NextCursor.Sequence != start.Sequence+uint64(len(verified.Events)) ||
			verified.HasMore != (verified.NextCursor.Sequence < member.Committed) {
			return retiredMemberReconcileReport{}, errors.New("retired member page sequence is inconsistent")
		}
		next := start
		for _, event := range verified.Events {
			if event.Sequence != next.Sequence+1 || event.PreviousDigest != next.Digest ||
				event.FromRole != next.Role || event.FromTerm != next.Term || event.FromPromisedTerm != next.PromisedTerm {
				return retiredMemberReconcileReport{}, errors.New("retired member page diverges from collector cursor")
			}
			next.Sequence, next.Digest = event.Sequence, event.Digest
			next.Role, next.Term, next.PromisedTerm = event.ToRole, event.ToTerm, event.ToPromisedTerm
		}
		if next.Digest != verified.NextCursor.Digest || next.Sequence != verified.NextCursor.Sequence ||
			!archive.capture(node, verified, next, time.Now().UTC()) {
			return retiredMemberReconcileReport{}, errors.New("collector refused authenticated retired member page")
		}
		result.ImportedEvents += uint64(len(verified.Events))
		start = next
		if !verified.HasMore {
			if start.Sequence != member.Committed || start.Digest != member.CommittedDigest {
				return retiredMemberReconcileReport{}, errors.New("retired member tail did not reach its authenticated committed head")
			}
			result.Status = "retired_member_tail_imported_new_generation_unapproved"
			result.CommittedSequence, result.CommittedDigest = member.Committed, member.CommittedDigest
			result.MemberInventorySHA256 = inventory
			return result, nil
		}
	}
}
