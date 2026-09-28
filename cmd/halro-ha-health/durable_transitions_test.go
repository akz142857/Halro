package main

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func durableTestPage() durableTransitionPage {
	page := durableTransitionPage{
		Version: 1, ClusterID: "ha", Incarnation: "inc_1", NodeID: "node-1",
		BaselineKind: "legacy_baseline", BaselineDigest: strings.Repeat("a", 64),
		BaselineRole: "replica", BaselineTerm: 7, BaselinePromised: 7,
		CommittedSequence: 1, HeadDigest: strings.Repeat("b", 64),
		Events: []durableTransitionEvent{{
			liveTransition: liveTransition{Sequence: 1, At: time.Now().UTC(), Kind: "promise",
				FromRole: "replica", ToRole: "replica", FromTerm: 7, ToTerm: 7,
				FromPromisedTerm: 7, ToPromisedTerm: 8},
			PreviousDigest: strings.Repeat("a", 64), Digest: strings.Repeat("b", 64),
		}},
	}
	page.NextCursor.JournalID = "0123456789abcdef0123456789abcdef"
	page.NextCursor.Sequence = 1
	page.NextCursor.Digest = strings.Repeat("b", 64)
	return page
}

func durableTestCollector(t *testing.T, respond func(*http.Request) (*http.Response, error)) statusCollector {
	t.Helper()
	path := filepath.Join(t.TempDir(), "status-token")
	if err := os.WriteFile(path, []byte("machine-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return statusCollector{config: statusMemberConfig{NodeID: "node-1", URL: "https://member.invalid/ha/status", TokenFile: path},
		client: &http.Client{Transport: roundTripFunc(respond)}}
}

func durableResponse(t *testing.T, page durableTransitionPage) *http.Response {
	t.Helper()
	encoded, err := json.Marshal(page)
	if err != nil {
		t.Fatal(err)
	}
	return jsonResponse(string(encoded))
}

func TestDurablePageCollectorValidatesIdentityAndCursor(t *testing.T) {
	page := durableTestPage()
	var calls int
	collector := durableTestCollector(t, func(request *http.Request) (*http.Response, error) {
		calls++
		if request.URL.Path != "/ha/transitions" || request.Header.Get("Authorization") != "Bearer machine-token" {
			t.Fatalf("unsafe durable request: %s %#v", request.URL, request.Header)
		}
		if calls == 1 {
			if request.URL.RawQuery != "" {
				t.Fatalf("first page sent a cursor: %s", request.URL.RawQuery)
			}
			return durableResponse(t, page), nil
		}
		if request.URL.Query().Get("after") != "1" || request.URL.Query().Get("digest") != page.NextCursor.Digest ||
			request.URL.Query().Get("journal_id") != page.NextCursor.JournalID {
			t.Fatalf("continuation lost cursor: %s", request.URL.RawQuery)
		}
		last := page
		last.Events = []durableTransitionEvent{}
		return durableResponse(t, last), nil
	})
	first, cursor, failure := collector.fetchDurablePage(context.Background(), "ha", "inc_1", nil)
	if failure != "" || len(first.Events) != 1 || cursor.Sequence != 1 || cursor.Role != "replica" || cursor.PromisedTerm != 8 {
		t.Fatalf("first page=%+v cursor=%+v failure=%s", first, cursor, failure)
	}
	second, next, failure := collector.fetchDurablePage(context.Background(), "ha", "inc_1", &cursor)
	if failure != "" || len(second.Events) != 0 || next != cursor || calls != 2 {
		t.Fatalf("continuation=%+v cursor=%+v failure=%s calls=%d", second, next, failure, calls)
	}
}

func TestDurablePageCollectorRejectsIncompleteOrContradictoryEvidence(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*durableTransitionPage)
		want   string
	}{
		{"wrong cluster", func(page *durableTransitionPage) { page.ClusterID = "other" }, "identity_or_schema"},
		{"wrong member", func(page *durableTransitionPage) { page.NodeID = "other" }, "identity_or_schema"},
		{"wrong incarnation", func(page *durableTransitionPage) { page.Incarnation = "other" }, "identity_or_schema"},
		{"missing digest", func(page *durableTransitionPage) { page.Events[0].Digest = "" }, "chain_mismatch"},
		{"wrong predecessor", func(page *durableTransitionPage) { page.Events[0].PreviousDigest = strings.Repeat("c", 64) }, "chain_mismatch"},
		{"wrong role", func(page *durableTransitionPage) { page.Events[0].FromRole = "primary" }, "chain_mismatch"},
		{"wrong next cursor", func(page *durableTransitionPage) { page.NextCursor.Sequence = 2 }, "identity_or_schema"},
		{"incomplete page", func(page *durableTransitionPage) { page.HasMore = true; page.CommittedSequence = 2 }, "identity_or_schema"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			page := durableTestPage()
			tc.mutate(&page)
			collector := durableTestCollector(t, func(*http.Request) (*http.Response, error) { return durableResponse(t, page), nil })
			_, _, failure := collector.fetchDurablePage(context.Background(), "ha", "inc_1", nil)
			if failure != tc.want {
				t.Fatalf("failure=%q want=%q", failure, tc.want)
			}
		})
	}
}

func TestDurablePageCollectorRejectsChangedBaselineAfterEmptyPage(t *testing.T) {
	page := durableTestPage()
	page.Events = nil
	page.CommittedSequence = 0
	page.HeadDigest = page.BaselineDigest
	page.NextCursor.Sequence = 0
	page.NextCursor.Digest = page.BaselineDigest
	collector := durableTestCollector(t, func(*http.Request) (*http.Response, error) { return durableResponse(t, page), nil })
	_, cursor, failure := collector.fetchDurablePage(context.Background(), "ha", "inc_1", nil)
	if failure != "" || cursor.Sequence != 0 {
		t.Fatalf("initial cursor=%+v failure=%s", cursor, failure)
	}
	cursor.Digest = strings.Repeat("c", 64)
	_, _, failure = collector.fetchDurablePage(context.Background(), "ha", "inc_1", &cursor)
	if failure != "chain_mismatch" {
		t.Fatalf("changed baseline digest accepted: %s", failure)
	}
}

func TestDurablePageCollectorRejectsRegressedTimestamp(t *testing.T) {
	page := durableTestPage()
	second := page.Events[0]
	second.Sequence = 2
	second.At = second.At.Add(-time.Second)
	second.FromPromisedTerm = 8
	second.ToPromisedTerm = 9
	second.PreviousDigest = page.Events[0].Digest
	second.Digest = strings.Repeat("c", 64)
	page.Events = append(page.Events, second)
	page.CommittedSequence = 2
	page.HeadDigest = second.Digest
	page.NextCursor.Sequence = 2
	page.NextCursor.Digest = second.Digest
	collector := durableTestCollector(t, func(*http.Request) (*http.Response, error) { return durableResponse(t, page), nil })
	_, _, failure := collector.fetchDurablePage(context.Background(), "ha", "inc_1", nil)
	if failure != "chain_mismatch" {
		t.Fatalf("regressed timestamp accepted: %s", failure)
	}
}
