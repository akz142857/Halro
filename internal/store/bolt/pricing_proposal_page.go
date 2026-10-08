package bolt

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"math"
	"slices"
	"strings"

	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/metadatajournal"
)

func migratePricingProposalRevision(tx *Tx, step func(string) error) error {
	if err := migrationStep(step, "before_proposal_revision"); err != nil {
		return err
	}
	if _, err := tx.CreateBucketIfNotExists(bucketPricingProposalRevision); err != nil {
		return err
	}
	return migrationStep(step, "after_proposal_revision")
}

func proposalRevisionTx(tx *Tx, dep string) (uint64, error) {
	b := tx.Bucket(bucketPricingProposalRevision)
	if b == nil {
		return 0, errors.New("pricing proposal revision bucket missing")
	}
	raw := b.Get([]byte(dep))
	if raw == nil {
		return 0, nil
	}
	v, ok := decodeUint64(raw)
	if !ok {
		return 0, errors.New("pricing proposal revision malformed")
	}
	return v, nil
}

func (r *recorder) trackProposalMutation(kind metadatajournal.OpKind, path []string, previous, value []byte) error {
	if len(path) == 0 {
		return nil
	}
	if path[0] == string(bucketPricingProposalRevision) {
		if !r.updatingProposalRevisions {
			return errors.New("proposal revision is owned by transaction entry")
		}
		return nil
	}
	if path[0] == string(bucketPricingProposalOrder) || path[0] == string(bucketPricingProposalCount) {
		if !r.updatingProposalOrder {
			return errors.New("proposal index is owned by transaction entry")
		}
		return nil
	}
	if path[0] != string(bucketDeploymentPriceProposals) {
		return nil
	}
	if len(path) != 1 || kind == metadatajournal.OpDeleteBucket {
		return errors.New("proposal root mutation requires whole-file publication")
	}
	for _, raw := range [][]byte{previous, value} {
		if raw == nil {
			continue
		}
		if len(raw) > metadatajournal.MaxPayloadSize {
			return ErrPricingAuditLimit
		}
		var p struct {
			DeploymentID string `json:"deployment_id"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return err
		}
		if strings.TrimSpace(p.DeploymentID) == "" {
			return errors.New("proposal mutation has no deployment")
		}
		if r.proposalDeployments == nil {
			r.proposalDeployments = make(map[string]struct{})
		}
		r.proposalDeployments[p.DeploymentID] = struct{}{}
	}
	return nil
}

func (tx *Tx) bumpProposalRevisions() error {
	ids := make([]string, 0, len(tx.recorder.proposalDeployments))
	for id := range tx.recorder.proposalDeployments {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	tx.recorder.updatingProposalRevisions = true
	defer func() { tx.recorder.updatingProposalRevisions = false }()
	for _, id := range ids {
		current, err := proposalRevisionTx(tx, id)
		if err != nil {
			return err
		}
		if current == math.MaxUint64 {
			return errors.New("proposal revision exhausted")
		}
		if err := tx.Bucket(bucketPricingProposalRevision).Put([]byte(id), encodeUint64(current+1)); err != nil {
			return err
		}
	}
	return nil
}

type DeploymentPriceProposalPage struct {
	Items      []domain.DeploymentPriceProposal `json:"items"`
	NextCursor string                           `json:"next_cursor"`
	Revision   uint64                           `json:"revision"`
}

// Fixed positions sort by created UTC time then SHA256(proposal ID), descending.
// The hash keeps continuation length fixed even for wide legacy native IDs.
// This is a stateless process-bound continuation, not a content snapshot.
type proposalPosition [44]byte

func proposalOrder(p domain.DeploymentPriceProposal) proposalPosition {
	var pos proposalPosition
	binary.BigEndian.PutUint64(pos[:8], uint64(p.CreatedAt.Unix())^(uint64(1)<<63))
	binary.BigEndian.PutUint32(pos[8:12], uint32(p.CreatedAt.Nanosecond()))
	digest := sha256.Sum256([]byte(p.ID))
	copy(pos[12:], digest[:])
	return pos
}

const proposalCursorBytes = 1 + 32 + 8 + 44 + 32

func (s *Store) decodeProposalCursor(cursor, dep string) (*proposalPosition, *uint64, error) {
	if cursor == "" {
		return nil, nil, nil
	}
	if len(cursor) > 160 {
		return nil, nil, ErrPricingPageCursor
	}
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil || len(raw) != proposalCursorBytes || raw[0] != 2 {
		return nil, nil, ErrPricingPageCursor
	}
	digest := sha256.Sum256([]byte(dep))
	mac := hmac.New(sha256.New, s.pricingPageKey[:])
	mac.Write(raw[:85])
	if !bytes.Equal(raw[1:33], digest[:]) || !hmac.Equal(raw[85:], mac.Sum(nil)) {
		return nil, nil, ErrPricingPageCursor
	}
	var pos proposalPosition
	copy(pos[:], raw[41:85])
	rev := binary.BigEndian.Uint64(raw[33:41])
	return &pos, &rev, nil
}
func (s *Store) encodeProposalCursor(dep string, revision uint64, pos proposalPosition) string {
	var raw [proposalCursorBytes]byte
	raw[0] = 2
	digest := sha256.Sum256([]byte(dep))
	copy(raw[1:33], digest[:])
	binary.BigEndian.PutUint64(raw[33:41], revision)
	copy(raw[41:85], pos[:])
	mac := hmac.New(sha256.New, s.pricingPageKey[:])
	mac.Write(raw[:85])
	mac.Sum(raw[:85])
	return base64.RawURLEncoding.EncodeToString(raw[:])
}

// A cold read validates the complete primary/index set with finite readers.
// Subsequent pages seek the native order under a fixed, revision-bound marker.
func (s *Store) DeploymentPriceProposalsPage(ctx context.Context, dep, cursor string, limit int) (page DeploymentPriceProposalPage, resultErr error) {
	if strings.TrimSpace(dep) == "" || limit < 1 || limit > 100 {
		return page, ErrPricingPageCursor
	}
	leave, err := s.enter()
	if err != nil {
		return page, err
	}
	defer leave()
	op, err := s.acquirePricingIO()
	if err != nil {
		return page, err
	}
	defer func() {
		resultErr = errors.Join(resultErr, op.release())
		if resultErr != nil {
			page = DeploymentPriceProposalPage{}
		}
	}()
	ctx, cancel := s.confirmationContext(ctx)
	defer cancel()
	s.pricingPageKeyOnce.Do(func() { _, s.pricingPageKeyErr = rand.Read(s.pricingPageKey[:]) })
	if s.pricingPageKeyErr != nil {
		return page, s.pricingPageKeyErr
	}
	after, expected, err := s.decodeProposalCursor(cursor, dep)
	if err != nil {
		return page, err
	}
	state := s.deploymentPricingState(dep)
	state.proposalMu.Lock()
	defer state.proposalMu.Unlock()
	var revision uint64
	if err := s.view(func(tx *Tx) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := proposalIndexReadyTx(tx); err != nil {
			return err
		}
		if tx.Bucket(bucketDeployments).Get([]byte(dep)) == nil {
			return ErrNotFound
		}
		var err error
		revision, err = proposalRevisionTx(tx, dep)
		if err != nil {
			return err
		}
		if expected != nil && revision != *expected {
			return ErrPricingPageChanged
		}
		return nil
	}); err != nil {
		return page, err
	}
	key := sha256.Sum256([]byte(dep))
	if !state.proposalLoaded || state.proposalKey != key || state.proposalRevision != revision {
		state.proposalLoaded = false
		if err := s.auditProposalOrder(ctx, dep, revision); err != nil {
			return page, err
		}
		state.proposalKey, state.proposalRevision, state.proposalLoaded = key, revision, true
	}
	source, err := s.readProposalOrderPage(ctx, dep, after, revision, limit)
	if err != nil {
		return page, err
	}
	page = DeploymentPriceProposalPage{Items: make([]domain.DeploymentPriceProposal, 0, len(source.rows)), Revision: revision}
	for _, row := range source.rows {
		if err := ctx.Err(); err != nil {
			return page, err
		}
		p, err := decodeIndexedProposal(dep, row)
		if err != nil {
			return page, err
		}
		page.Items = append(page.Items, p)
	}
	if !source.done {
		if len(source.rows) == 0 {
			return page, ErrProposalIndexConflict
		}
		page.NextCursor = s.encodeProposalCursor(dep, revision, source.rows[len(source.rows)-1].pos)
	}
	if err := s.view(func(tx *Tx) error { return checkProposalRevision(ctx, tx, dep, revision) }); err != nil {
		return page, err
	}
	return page, ctx.Err()
}
