package governance

import (
	"container/heap"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"sort"
	"strings"

	"github.com/akz142857/Halro/internal/domain"
)

var (
	ErrOutcomeCursor    = errors.New("invalid or expired outcome cursor")
	ErrOutcomePageLimit = errors.New("outcome page limit is invalid or too small")
)

const OutcomePageMaxRows = 200
const OutcomePageMaxBytes = 1 << 20
const outcomeCursorPrefix = "gop1."

type OutcomePageQuery struct {
	ProjectID, WorkUnitID, DefinitionID string
	Cursor                              string
	Rows, Bytes                         int
}

type OutcomePage struct {
	Items      []domain.Outcome
	NextCursor string
	Head       Summary
	Bytes      int
}

type outcomeHeap []domain.Outcome

func (h outcomeHeap) Len() int           { return len(h) }
func (h outcomeHeap) Less(i, j int) bool { return h[i].ID > h[j].ID }
func (h outcomeHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *outcomeHeap) Push(value any)    { *h = append(*h, value.(domain.Outcome)) }
func (h *outcomeHeap) Pop() any {
	old := *h
	value := old[len(old)-1]
	old[len(old)-1] = domain.Outcome{}
	*h = old[:len(old)-1]
	return value
}

func outcomeScope(q OutcomePageQuery) [32]byte {
	// Length-prefixed query fields prevent ambiguous scope encodings. Page
	// size may change across continuations without changing their fact prefix.
	h := sha256.New()
	for _, value := range []string{q.ProjectID, q.WorkUnitID, q.DefinitionID} {
		h.Write(binary.BigEndian.AppendUint64(nil, uint64(len(value))))
		h.Write([]byte(value))
	}
	var result [32]byte
	copy(result[:], h.Sum(nil))
	return result
}

func outcomeCursorMAC(key, payload []byte) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte("halro:outcome-page:v1"))
	h.Write(payload)
	return h.Sum(nil)
}

// Called with the Manager mutex held; shared only as ephemeral owner entropy.
func (m *Manager) ensurePageKey() error {
	if len(m.pageKey) != 0 {
		return nil
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		clear(key)
		return errors.Join(ErrUnavailable, err)
	}
	m.pageKey = key
	return nil
}

func encodeOutcomeCursor(key []byte, scope [32]byte, head Summary, after string) string {
	raw := binary.BigEndian.AppendUint64(nil, head.Records)
	raw = binary.BigEndian.AppendUint64(raw, uint64(head.Bytes))
	raw = append(raw, head.LastHash[:]...)
	raw = append(raw, scope[:]...)
	raw = append(raw, after...)
	raw = append(raw, outcomeCursorMAC(key, raw)...)
	return outcomeCursorPrefix + base64.RawURLEncoding.EncodeToString(raw)
}

func decodeOutcomeCursor(key []byte, scope [32]byte, cursor string, current Summary) (Summary, string, error) {
	if cursor == "" {
		return current, "", nil
	}
	if len(cursor) > 400 {
		return Summary{}, "", ErrOutcomeCursor
	}
	if !strings.HasPrefix(cursor, outcomeCursorPrefix) {
		// Compatibility: an old ID-only cursor starts a new current fact cut.
		// It grants no prefix authority; all new continuations are owner-signed.
		raw, err := base64.RawURLEncoding.DecodeString(cursor)
		if err != nil || len(raw) == 0 || len(raw) > 256 || strings.ContainsRune(string(raw), 0) {
			return Summary{}, "", ErrOutcomeCursor
		}
		return current, string(raw), nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(cursor, outcomeCursorPrefix))
	if err != nil || len(raw) <= 112 || len(raw) > 112+domain.MaxGovernanceIDLength {
		return Summary{}, "", ErrOutcomeCursor
	}
	payload, signature := raw[:len(raw)-32], raw[len(raw)-32:]
	if !hmac.Equal(signature, outcomeCursorMAC(key, payload)) || !hmac.Equal(payload[48:80], scope[:]) {
		return Summary{}, "", ErrOutcomeCursor
	}
	head := Summary{Records: binary.BigEndian.Uint64(payload[:8]), Bytes: int64(binary.BigEndian.Uint64(payload[8:16]))}
	copy(head.LastHash[:], payload[16:48])
	after := string(payload[80:])
	if !domain.ValidOutcomeID(after) || head.Records == 0 || head.Bytes <= 0 || head.Records > current.Records || head.Bytes > current.Bytes || head.Records == current.Records && head != current {
		return Summary{}, "", ErrOutcomeCursor
	}
	return head, after, nil
}

// This legacy adapter retains only Rows+1 candidates; it does not copy/sort
// all historical Outcome rows. Historical State residency and scanning CPU
// remain migration work. Native Outcome rows are immutable at their sequence.
func (s *State) readOutcomePage(ctx context.Context, q OutcomePageQuery, after string, head Summary) (OutcomePage, string, error) {
	page := OutcomePage{Items: make([]domain.Outcome, 0), Head: head}
	if s == nil {
		return OutcomePage{}, "", ErrUnavailable
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if head.Records > s.sequence {
		return OutcomePage{}, "", ErrUnavailable
	}
	candidates := make(outcomeHeap, 0, q.Rows+1)
	for id, row := range s.outcomes {
		if err := ctx.Err(); err != nil {
			return OutcomePage{}, "", err
		}
		if id != row.ID || row.GovernanceSequence == 0 || row.GovernanceSequence > s.sequence || row.Validate() != nil {
			return OutcomePage{}, "", ErrCorrupt
		}
		if id <= after || row.GovernanceSequence > head.Records || q.ProjectID != "" && row.ProjectID != q.ProjectID || q.WorkUnitID != "" && row.WorkUnitID != q.WorkUnitID || q.DefinitionID != "" && row.DefinitionID != q.DefinitionID {
			continue
		}
		if len(candidates) < q.Rows+1 {
			heap.Push(&candidates, row)
		} else if id < candidates[0].ID {
			candidates[0] = row
			heap.Fix(&candidates, 0)
		}
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].ID < candidates[j].ID })
	next := ""
	for i, row := range candidates {
		if err := ctx.Err(); err != nil {
			return OutcomePage{}, "", err
		}
		if i == q.Rows {
			next = page.Items[len(page.Items)-1].ID
			break
		}
		// Refuse impossible source sizes before allocating an encoded copy.
		sourceBytes := len(row.ID) + len(row.ProjectID) + len(row.WorkUnitID) + len(row.DefinitionID) + len(row.Value) + len(row.ReporterKeyID) + len(row.EvidenceSHA256) + len(row.EvidenceRef) + len(row.SupersedesOutcomeID)
		if sourceBytes > OutcomePageMaxBytes {
			return OutcomePage{}, "", ErrOutcomePageLimit
		}
		raw, err := json.Marshal(row)
		if err != nil {
			return OutcomePage{}, "", err
		}
		if len(raw) > q.Bytes-page.Bytes {
			if len(page.Items) == 0 {
				return OutcomePage{}, "", ErrOutcomePageLimit
			}
			next = page.Items[len(page.Items)-1].ID
			break
		}
		page.Bytes += len(raw)
		page.Items = append(page.Items, row)
	}
	if err := ctx.Err(); err != nil {
		return OutcomePage{}, "", err
	}
	return page, next, nil
}

// ReadOutcomePage freezes native facts across client continuations. The
// ephemeral owner key binds scope and prefix and invalidates cursors on a new
// Manager. It is NOT authentication of a persisted State or DB checkpoint.
// Provisional accounting is a separate, current per-page observation.
func (m *Manager) ReadOutcomePage(ctx context.Context, q OutcomePageQuery) (OutcomePage, error) {
	ctx, release, err := m.enterOperation(ctx)
	if err != nil {
		return OutcomePage{}, err
	}
	defer release()
	if err := ctx.Err(); err != nil {
		return OutcomePage{}, err
	}
	if q.Rows < 1 || q.Rows > OutcomePageMaxRows || q.Bytes < 1 || q.Bytes > OutcomePageMaxBytes || len(q.ProjectID) > maxPayloadSize || len(q.WorkUnitID) > domain.MaxGovernanceIDLength || len(q.DefinitionID) > domain.MaxGovernanceIDLength {
		return OutcomePage{}, ErrOutcomePageLimit
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	current, err := m.ownerHead(ctx)
	if err != nil {
		return OutcomePage{}, err
	}
	if err := m.ensurePageKey(); err != nil {
		return OutcomePage{}, err
	}
	scope := outcomeScope(q)
	head, after, err := decodeOutcomeCursor(m.pageKey, scope, q.Cursor, current)
	if err != nil {
		return OutcomePage{}, err
	}
	var page OutcomePage
	var next string
	if m.projection != nil {
		page, next, err = m.projection.readOutcomePage(ctx, q, after, head)
	} else {
		page, next, err = m.state.readOutcomePage(ctx, q, after, head)
	}
	if err != nil {
		return OutcomePage{}, err
	}
	if next != "" {
		page.NextCursor = encodeOutcomeCursor(m.pageKey, scope, head, next)
	}
	return page, nil
}
