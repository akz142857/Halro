package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type durableTransitionCursor struct {
	JournalID      string `json:"journal_id"`
	Sequence       uint64 `json:"sequence"`
	Digest         string `json:"digest"`
	BaselineDigest string `json:"baseline_digest"`
	Role           string `json:"role"`
	Term           uint64 `json:"term"`
	PromisedTerm   uint64 `json:"promised_term"`
}

type durableTransitionEvent struct {
	liveTransition
	PreviousDigest string `json:"previous_digest"`
	Digest         string `json:"digest"`
}

type durableTransitionPage struct {
	Version           int                      `json:"version"`
	ClusterID         string                   `json:"cluster_id"`
	Incarnation       string                   `json:"incarnation"`
	NodeID            string                   `json:"node_id"`
	BaselineKind      string                   `json:"baseline_kind"`
	BaselineDigest    string                   `json:"baseline_digest"`
	BaselineRole      string                   `json:"baseline_role"`
	BaselineTerm      uint64                   `json:"baseline_term"`
	BaselinePromised  uint64                   `json:"baseline_promised_term"`
	CommittedSequence uint64                   `json:"committed_sequence"`
	HeadDigest        string                   `json:"head_digest"`
	Events            []durableTransitionEvent `json:"events"`
	NextCursor        struct {
		JournalID string `json:"journal_id"`
		Sequence  uint64 `json:"sequence"`
		Digest    string `json:"digest"`
	} `json:"next_cursor"`
	HasMore bool `json:"has_more"`
}

func validDurableDigest(value string) bool {
	if len(value) != 64 || strings.ToLower(value) != value {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func validDurableJournalID(value string) bool {
	if len(value) != 32 || strings.ToLower(value) != value {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func validDurableRole(value string) bool {
	return value == "primary" || value == "replica" || value == "awaiting_decision"
}

func validDurableChange(event liveTransition) bool {
	if !validDurableRole(event.FromRole) || !validDurableRole(event.ToRole) ||
		event.FromPromisedTerm < event.FromTerm || event.ToPromisedTerm < event.ToTerm {
		return false
	}
	switch event.Kind {
	case "promise":
		role := event.FromRole
		if role == "primary" {
			role = "replica"
		}
		return event.ToRole == role && event.ToTerm == event.FromTerm && event.ToPromisedTerm > event.FromPromisedTerm
	case "promote":
		return event.FromRole == "replica" && event.ToRole == "primary" &&
			event.ToTerm == event.FromPromisedTerm && event.ToTerm > event.FromTerm &&
			event.ToPromisedTerm == event.FromPromisedTerm
	case "adopt_higher_term":
		return event.ToRole == "replica" && event.ToTerm >= event.FromTerm &&
			event.ToPromisedTerm >= event.FromPromisedTerm &&
			(event.ToRole != event.FromRole || event.ToTerm != event.FromTerm || event.ToPromisedTerm != event.FromPromisedTerm)
	default:
		return false
	}
}

// fetchDurablePage only validates a member's machine-only page. Its caller
// must persist the returned cursor together with the records before using it
// for a later page or making a completeness claim.
func (c statusCollector) fetchDurablePage(ctx context.Context, cluster, incarnation string, cursor *durableTransitionCursor) (durableTransitionPage, durableTransitionCursor, string) {
	var empty durableTransitionPage
	var emptyCursor durableTransitionCursor
	base, err := url.Parse(c.config.URL)
	if err != nil || base == nil || base.Scheme != "https" || base.EscapedPath() != "/ha/status" || base.RawQuery != "" {
		return empty, emptyCursor, "configuration"
	}
	base.Path, base.RawPath = "/ha/transitions", ""
	if cursor != nil {
		if !validDurableJournalID(cursor.JournalID) || !validDurableDigest(cursor.Digest) || !validDurableDigest(cursor.BaselineDigest) ||
			!validDurableRole(cursor.Role) || cursor.PromisedTerm < cursor.Term {
			return empty, emptyCursor, "configuration"
		}
	}
	if cursor != nil && cursor.Sequence > 0 {
		query := url.Values{}
		query.Set("after", strconv.FormatUint(cursor.Sequence, 10))
		query.Set("digest", cursor.Digest)
		query.Set("journal_id", cursor.JournalID)
		base.RawQuery = query.Encode()
	}
	token, err := loadMachineStatusToken(c.config.TokenFile)
	if err != nil {
		return empty, emptyCursor, "credential_unavailable"
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, base.String(), nil)
	if err != nil {
		return empty, emptyCursor, "configuration"
	}
	request.Header.Set("Authorization", "Bearer "+token)
	response, err := c.client.Do(request)
	if err != nil {
		return empty, emptyCursor, "transport"
	}
	defer response.Body.Close()
	switch response.StatusCode {
	case http.StatusOK:
	case http.StatusUnauthorized, http.StatusForbidden:
		return empty, emptyCursor, "authentication"
	case http.StatusConflict:
		return empty, emptyCursor, "cursor_conflict"
	case http.StatusServiceUnavailable:
		return empty, emptyCursor, "unavailable"
	default:
		return empty, emptyCursor, "http_status"
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, (64<<10)+1))
	if err != nil || len(body) > 64<<10 {
		return empty, emptyCursor, "response_size"
	}
	decoder := json.NewDecoder(strings.NewReader(string(body)))
	decoder.DisallowUnknownFields()
	var page durableTransitionPage
	if err := decoder.Decode(&page); err != nil || !errors.Is(decoder.Decode(new(any)), io.EOF) {
		return empty, emptyCursor, "identity_or_schema"
	}
	if page.Version != 1 || page.ClusterID != cluster || page.NodeID != c.config.NodeID || page.Incarnation != incarnation ||
		(page.BaselineKind != "legacy_baseline" && page.BaselineKind != "initial_state") ||
		!validDurableJournalID(page.NextCursor.JournalID) || !validDurableDigest(page.BaselineDigest) ||
		!validDurableDigest(page.HeadDigest) || !validDurableDigest(page.NextCursor.Digest) ||
		!validDurableRole(page.BaselineRole) || page.BaselinePromised < page.BaselineTerm ||
		len(page.Events) > 64 || page.NextCursor.Sequence > page.CommittedSequence ||
		page.HasMore != (page.NextCursor.Sequence < page.CommittedSequence) ||
		page.HasMore && len(page.Events) != 64 {
		return empty, emptyCursor, "identity_or_schema"
	}
	start := durableTransitionCursor{
		JournalID: page.NextCursor.JournalID, Sequence: 0, Digest: page.BaselineDigest,
		BaselineDigest: page.BaselineDigest, Role: page.BaselineRole, Term: page.BaselineTerm, PromisedTerm: page.BaselinePromised,
	}
	if cursor != nil {
		if cursor.JournalID != start.JournalID || cursor.BaselineDigest != start.BaselineDigest {
			return empty, emptyCursor, "chain_mismatch"
		}
		if cursor.Sequence == 0 && (cursor.Digest != start.Digest || cursor.Role != start.Role ||
			cursor.Term != start.Term || cursor.PromisedTerm != start.PromisedTerm) {
			return empty, emptyCursor, "chain_mismatch"
		}
		start = *cursor
	}
	if start.Sequence > ^uint64(0)-uint64(len(page.Events)) {
		return empty, emptyCursor, "chain_mismatch"
	}
	latest := start
	var previousAt time.Time
	for _, event := range page.Events {
		if event.Sequence != latest.Sequence+1 || event.PreviousDigest != latest.Digest ||
			!validDurableDigest(event.Digest) || !validDurableChange(event.liveTransition) ||
			event.FromRole != latest.Role || event.FromTerm != latest.Term || event.FromPromisedTerm != latest.PromisedTerm ||
			event.At.IsZero() || event.At.Before(previousAt) || event.At.After(time.Now().Add(30*time.Second)) {
			return empty, emptyCursor, "chain_mismatch"
		}
		previousAt = event.At
		latest.Sequence, latest.Digest = event.Sequence, event.Digest
		latest.Role, latest.Term, latest.PromisedTerm = event.ToRole, event.ToTerm, event.ToPromisedTerm
	}
	if latest.Sequence != page.NextCursor.Sequence || latest.Digest != page.NextCursor.Digest ||
		page.CommittedSequence == latest.Sequence && page.HeadDigest != latest.Digest {
		return empty, emptyCursor, "chain_mismatch"
	}
	return page, latest, ""
}
