package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/openaiapi"
	"github.com/akz142857/Halro/internal/provider"
	"github.com/akz142857/Halro/internal/safelog"
	"github.com/akz142857/Halro/internal/semantic"
	"github.com/akz142857/Halro/internal/usage"
)

// The word "failure" means four different things along one request, and the
// console, the Admin API and the process log each have to answer with the same
// one. This file fixes which is which, by observing all four counts for the
// same run:
//
//   - attempt failures — one per upstream call that did not complete, written
//     as WARN. A request that falls back after one is still a success.
//   - final request failures — RequestFinalized with an outcome other than
//     success. This is what the summary card's "N failed" counts.
//   - final failure ERROR records — deliberately a subset of the line above.
//   - pre-admission failures — none of the above, because they return before a
//     ledger request exists at all.
//
// Neither of the last two can be derived from the name "final failure": the
// first is a capacity decision, the second is where beginRequestRun sits in the
// admission order. Both are read out of the code here so the slices that follow
// do not each re-derive them.

// failureCounts is what one scenario produced, across every view that reports
// on failure.
type failureCounts struct {
	// outcome of the RequestFinalized event, or "" when none was written.
	outcome string
	// requestErrors is what the usage aggregate — the source the summary card
	// reads — counts after replaying the whole WAL.
	requestErrors uint64
	// attemptWarnings counts "provider attempt failed" WARN records.
	attemptWarnings int
	// finalFailureErrors counts "request failed" ERROR records.
	finalFailureErrors int
}

func runFailureScenario(t *testing.T, f *fixture, call func() error) failureCounts {
	t.Helper()
	logs := &bytes.Buffer{}
	f.service.logger = safelog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	_ = call()

	counts := failureCounts{}
	aggregate := usage.NewAggregate()
	if _, err := f.log.Replay(ledger.Watermark{}, func(record ledger.Record) error {
		if record.Event.Kind == ledger.EventRequestFinalized {
			counts.outcome = record.Event.Outcome
		}
		return aggregate.Apply(record)
	}); err != nil {
		t.Fatal(err)
	}
	counts.requestErrors = aggregate.Metrics().RequestsError

	for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
		if line == "" {
			continue
		}
		var record struct {
			Level   string `json:"level"`
			Message string `json:"msg"`
		}
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("log line is not JSON: %s", line)
		}
		switch {
		case record.Message == "provider attempt failed" && record.Level == "WARN":
			counts.attemptWarnings++
		case record.Message == "request failed" && record.Level == "ERROR":
			counts.finalFailureErrors++
		}
	}
	return counts
}

// registerFallback adds a second target behind the same alias, at a lower
// priority than the fixture's own, so the primary is tried first.
func registerFallback(t *testing.T, f *fixture, adapter *fakeAdapter) {
	t.Helper()
	if err := f.registry.Register(provider.Target{
		ID: "target_2", DeploymentID: "dep_target_2", PublicModel: "chat",
		ProviderModel: "provider-model", Adapter: adapter, Priority: 1,
		InputMicrosPerMillion: 1_000_000, OutputMicrosPerMillion: 2_000_000,
	}); err != nil {
		t.Fatal(err)
	}
}

func TestFailureTaxonomyIsTheSameInEveryView(t *testing.T) {
	cases := []struct {
		name string
		// dailyBudget is the fixture's, because one scenario needs a budget too
		// small to reserve an attempt against.
		dailyBudget int64
		scenario    func(t *testing.T, f *fixture) func() error
		want        failureCounts
	}{
		{
			// The baseline. Without it every assertion below is satisfied by a
			// gateway that fails everything.
			name:        "success",
			dailyBudget: 1_000_000,
			scenario: func(_ *testing.T, f *fixture) func() error {
				return func() error {
					_, err := f.service.Chat(context.Background(), f.plaintext, chatRequest())
					return err
				}
			},
			want: failureCounts{outcome: "success"},
		},
		{
			name:        "single non-retryable provider failure",
			dailyBudget: 1_000_000,
			scenario: func(_ *testing.T, f *fixture) func() error {
				f.adapter.err = &provider.Error{
					Class: provider.ErrorBadRequest, Retryable: false, StatusCode: 400,
					Message: "invalid request",
				}
				return func() error {
					_, err := f.service.Chat(context.Background(), f.plaintext, chatRequest())
					return err
				}
			},
			want: failureCounts{outcome: "provider_error", requestErrors: 1, attemptWarnings: 1, finalFailureErrors: 1},
		},
		{
			// The contract this whole file exists for: a request that fell back
			// and then answered is a success. It leaves an attempt failure
			// behind, and that attempt failure is not a failed request.
			name:        "failure then fallback success",
			dailyBudget: 1_000_000,
			scenario: func(t *testing.T, f *fixture) func() error {
				f.adapter.err = &provider.Error{
					Class: provider.ErrorProvider5xx, Retryable: true, StatusCode: 503,
					Message: "primary unavailable",
				}
				registerFallback(t, f, &fakeAdapter{response: openaiapi.ChatCompletionResponse{
					ID: "chatcmpl_fallback", Object: "chat.completion", Model: "provider-model",
					Choices: []openaiapi.Choice{{Index: 0}},
					Usage:   &openaiapi.Usage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15},
				}})
				return func() error {
					_, err := f.service.Chat(context.Background(), f.plaintext, chatRequest())
					return err
				}
			},
			// Two warnings: the primary is retried against itself once before
			// the fallback is reached.
			want: failureCounts{outcome: "success", attemptWarnings: 2},
		},
		{
			name:        "every target fails",
			dailyBudget: 1_000_000,
			scenario: func(t *testing.T, f *fixture) func() error {
				failure := &provider.Error{
					Class: provider.ErrorProvider5xx, Retryable: true, StatusCode: 503,
					Message: "unavailable",
				}
				f.adapter.err = failure
				registerFallback(t, f, &fakeAdapter{err: failure})
				return func() error {
					_, err := f.service.Chat(context.Background(), f.plaintext, chatRequest())
					return err
				}
			},
			want: failureCounts{outcome: "provider_error", requestErrors: 1, attemptWarnings: 3, finalFailureErrors: 1},
		},
		{
			// The upstream answered; the answer could not be put on the
			// caller's wire. There is no attempt failure to warn about, and the
			// request still failed.
			name:        "response render failure",
			dailyBudget: 1_000_000,
			scenario: func(t *testing.T, f *fixture) func() error {
				return func() error {
					_, err := f.service.generate(
						context.Background(), f.plaintext, "chat", chatCanonical(t),
						func(semantic.GenerateResult) error {
							return errors.New("wire form cannot carry this content kind")
						},
					)
					return err
				}
			},
			want: failureCounts{outcome: "provider_error", requestErrors: 1, finalFailureErrors: 1},
		},
		{
			// The caller went away. It is a failed request in the ledger, but
			// nothing upstream faltered.
			name:        "client cancellation",
			dailyBudget: 1_000_000,
			scenario: func(_ *testing.T, f *fixture) func() error {
				f.adapter.err = context.Canceled
				return func() error {
					_, err := f.service.Chat(context.Background(), f.plaintext, chatRequest())
					return err
				}
			},
			want: failureCounts{outcome: "provider_error", requestErrors: 1, attemptWarnings: 1, finalFailureErrors: 1},
		},
		{
			// Admitted into the ledger, then refused before any upstream call:
			// one micro-USD of daily budget accepts a request and can never
			// reserve an attempt against it. This is the "rejected" terminal
			// state, and it counts as a failed request with no provider,
			// no deployment and no error class to explain it.
			name:        "post-admission rejection: budget exhausted",
			dailyBudget: 1,
			scenario: func(_ *testing.T, f *fixture) func() error {
				return func() error {
					_, err := f.service.Chat(context.Background(), f.plaintext, chatRequest())
					return err
				}
			},
			want: failureCounts{outcome: "rejected", requestErrors: 1},
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			f := newFixture(t, testCase.dailyBudget)
			defer f.close()
			call := testCase.scenario(t, &f)
			got := runFailureScenario(t, &f, call)
			if got != testCase.want {
				t.Fatalf("counts = %+v, want %+v", got, testCase.want)
			}
		})
	}
}

// A pre-admission failure is the blind spot this design accepts on purpose, and
// the one an operator meets most often: an invalid key is a 401 that never
// reaches beginRequestRun, so it writes no ledger request, changes no
// request_errors, and will never appear in a list built from RequestFinalized.
//
// The reason it stays out is that the ledger is the accounting authority. A
// request that never took budget and never chose a target has nothing to
// account for, and filing it there would make the billing truth source keep
// records for traffic it never admitted. The cost is that the console cannot
// say "all failures" — only "all admitted failures" — which is why this is
// asserted rather than explained.
func TestPreAdmissionFailureWritesNoLedgerRequest(t *testing.T) {
	f := newFixture(t, 1_000_000)
	defer f.close()
	counts := runFailureScenario(t, &f, func() error {
		_, err := f.service.Chat(context.Background(), "gw_not_a_real_key", chatRequest())
		if err == nil {
			t.Fatal("an invalid key was accepted")
		}
		return err
	})
	if counts != (failureCounts{}) {
		t.Fatalf("a pre-admission failure reached the ledger or the log: %+v", counts)
	}
}

// The same for a model the project is not routed to: a 404 the router answers
// before admission.
func TestUnroutedModelWritesNoLedgerRequest(t *testing.T) {
	f := newFixture(t, 1_000_000)
	defer f.close()
	request := chatRequest()
	request.Model = "not-a-model"
	counts := runFailureScenario(t, &f, func() error {
		_, err := f.service.Chat(context.Background(), f.plaintext, request)
		if err == nil {
			t.Fatal("an unrouted model was accepted")
		}
		return err
	})
	if counts != (failureCounts{}) {
		t.Fatalf("an unrouted model reached the ledger or the log: %+v", counts)
	}
}
