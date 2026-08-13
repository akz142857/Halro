package anthropic

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/akz142857/Halro/internal/anthropicapi"
	"github.com/akz142857/Halro/internal/provider"

	"github.com/akz142857/Halro/internal/compatibility"
	anthropicwire "github.com/akz142857/Halro/internal/compatibility/anthropic"
	openaiwire "github.com/akz142857/Halro/internal/compatibility/openai"
	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/openaiapi"
)

// batchRequest is one entry of Anthropic's inline batch body.
type batchRequest struct {
	CustomID string          `json:"custom_id"`
	Params   json.RawMessage `json:"params"`
}

// maxBatchInputLines bounds how many requests one batch may carry. The gateway
// already bounds the upload that produced them, but a bound on bytes is not a
// bound on work: a file of very short lines is small and still asks Halro to
// render tens of thousands of requests inside one HTTP handler.
const maxBatchInputLines = 100_000

// maxBatchInputLineBytes bounds one line. bufio.Scanner refuses longer ones
// anyway; naming the limit turns "token too long" into something an operator can
// act on.
const maxBatchInputLineBytes = 1 << 20

// renderBatchRequests converts the OpenAI-shaped JSONL a caller uploaded into
// Anthropic's inline request array.
//
// Each line is decoded through the same canonical model a live request takes and
// rendered by the same function, rather than through a second mapping written
// for batches. A batch line is a chat request that happens to be written down;
// giving it its own translation would let the two drift, and the one that drifts
// silently is the one nobody watches.
//
// A line the target cannot represent fails the whole batch and says which line.
// Capability filtering happens when a request is routed, and a batch is routed
// once for many requests, so this is where an unrepresentable field is caught.
// Dropping it and continuing would submit a batch the caller did not write.
func renderBatchRequests(profileID domain.ProviderProfileID, providerModel string, input []byte) ([]byte, error) {
	if len(bytes.TrimSpace(input)) == 0 {
		return nil, errors.New("batch input is empty")
	}
	scanner := bufio.NewScanner(bytes.NewReader(input))
	scanner.Buffer(make([]byte, 0, 64<<10), maxBatchInputLineBytes)
	requests := make([]batchRequest, 0, 64)
	seen := make(map[string]struct{}, 64)
	for line := 0; scanner.Scan(); {
		line++
		text := bytes.TrimSpace(scanner.Bytes())
		if len(text) == 0 {
			continue
		}
		if len(requests) == maxBatchInputLines {
			return nil, fmt.Errorf("batch input exceeds %d requests", maxBatchInputLines)
		}
		entry, err := renderBatchLine(profileID, providerModel, text)
		if err != nil {
			return nil, fmt.Errorf("batch input line %d: %w", line, err)
		}
		if _, duplicate := seen[entry.CustomID]; duplicate {
			return nil, fmt.Errorf("batch input line %d: custom_id %q appears more than once", line, entry.CustomID)
		}
		seen[entry.CustomID] = struct{}{}
		requests = append(requests, entry)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read batch input: %w", err)
	}
	if len(requests) == 0 {
		return nil, errors.New("batch input carries no requests")
	}
	return json.Marshal(map[string]any{"requests": requests})
}

// batchInputLine is the OpenAI batch input shape: an identifier, the endpoint
// the request would have been sent to, and the request itself.
type batchInputLine struct {
	CustomID string                          `json:"custom_id"`
	Method   string                          `json:"method"`
	URL      string                          `json:"url"`
	Body     openaiapi.ChatCompletionRequest `json:"body"`
}

func renderBatchLine(profileID domain.ProviderProfileID, providerModel string, text []byte) (batchRequest, error) {
	decoder := json.NewDecoder(bytes.NewReader(text))
	decoder.DisallowUnknownFields()
	var line batchInputLine
	if err := decoder.Decode(&line); err != nil {
		return batchRequest{}, fmt.Errorf("is not a batch input line: %w", err)
	}
	if strings.TrimSpace(line.CustomID) == "" {
		return batchRequest{}, errors.New("custom_id is required")
	}
	// The endpoint is part of the line the caller wrote, and this profile serves
	// exactly one of them. Accepting a line addressed elsewhere would run it
	// against a surface the caller did not choose.
	if line.URL != "" && line.URL != "/v1/chat/completions" {
		return batchRequest{}, fmt.Errorf("url %q is not served by this batch profile", line.URL)
	}
	if line.Method != "" && !strings.EqualFold(line.Method, "POST") {
		return batchRequest{}, fmt.Errorf("method %q is not POST", line.Method)
	}
	canonical, err := openaiwire.DecodeGenerate(line.Body)
	if err != nil {
		return batchRequest{}, fmt.Errorf("request is not representable: %w", err)
	}
	if canonical.Stream {
		return batchRequest{}, errors.New("a batched request cannot stream")
	}
	// Rendering alone does not refuse a field this profile cannot carry — it
	// renders what it can and leaves the rest behind. For a live request that is
	// harmless, because capability filtering has already routed the request away
	// from a target that would lose something. A batch is routed once for many
	// requests, so nothing has looked at these lines, and rendering them without
	// this check would silently drop members the caller wrote.
	if unsupported := compatibility.UnsupportedGenerateFields(profileID, canonical); len(unsupported) > 0 {
		return batchRequest{}, fmt.Errorf("request uses %s, which this provider cannot carry", strings.Join(unsupported, ", "))
	}
	rendered, err := anthropicwire.RenderPortableRequest(canonical, providerModel)
	if err != nil {
		return batchRequest{}, fmt.Errorf("request cannot be carried by this provider: %w", err)
	}
	// The batch body owns the model and the stream flag the same way a single
	// request does, so the rendered request keeps them and nothing here rewrites
	// them a second time.
	params, err := json.Marshal(rendered)
	if err != nil {
		return batchRequest{}, err
	}
	return batchRequest{CustomID: line.CustomID, Params: params}, nil
}

// batchRequestCounts is Anthropic's per-request tally. It stays inside this
// package: the northbound batch object declares no counts field
// (`batchResponseFields` in internal/compatibility/manifest.go), so these
// numbers decide a status and are not themselves reported.
type batchRequestCounts struct {
	Succeeded  int64 `json:"succeeded"`
	Errored    int64 `json:"errored"`
	Cancelled  int64 `json:"canceled"`
	Expired    int64 `json:"expired"`
	Processing int64 `json:"processing"`
}

// decodeBatchProcessingStatus maps Anthropic's batch lifecycle onto the OpenAI
// shape the northbound surface speaks.
//
// `ended` is not a synonym for success. It says the batch stopped, and the
// counts say how it stopped: a batch whose requests all errored has ended just
// as surely as one that succeeded, and reporting both as completed would tell
// the caller their work is ready when none of it is.
func decodeBatchProcessingStatus(status string, counts batchRequestCounts) string {
	switch status {
	case "in_progress":
		return "in_progress"
	case "canceling":
		return "cancelling"
	case "ended":
		switch {
		case counts.Cancelled > 0 && counts.Succeeded == 0:
			return "cancelled"
		case counts.Expired > 0 && counts.Succeeded == 0:
			return "expired"
		case counts.Succeeded == 0 && counts.Errored > 0:
			return "failed"
		default:
			return "completed"
		}
	default:
		// An unrecognised lifecycle value is not flattened into a familiar one.
		// A caller acting on "completed" would collect results that may not
		// exist; leaving it verbatim is visibly strange, which is the point.
		return status
	}
}

// batchesPath addresses the batch collection under whatever base this adapter
// was configured with. Built here rather than taken from a response: results_url
// arrives in the batch object, and dialling a URL the upstream chose would let
// the upstream decide which host Halro connects to — the one thing
// SafeTransport exists to prevent.
func (adapter *Adapter) batchesPath(suffix string) url.URL {
	endpoint := *adapter.endpoint
	endpoint.Path = strings.TrimRight(endpoint.Path, "/")
	if adapter.messagesPath != "" {
		endpoint.Path += "/" + strings.Trim(adapter.messagesPath, "/")
	} else {
		if !strings.HasSuffix(endpoint.Path, "/v1") {
			endpoint.Path += "/v1"
		}
		endpoint.Path += "/messages"
	}
	endpoint.Path += "/batches"
	if suffix != "" {
		endpoint.Path += "/" + suffix
	}
	return endpoint
}

func (adapter *Adapter) newBatchRequest(ctx context.Context, method, suffix, requestID string, body []byte) (*http.Request, error) {
	endpoint := adapter.batchesPath(suffix)
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	request, err := http.NewRequestWithContext(ctx, method, endpoint.String(), reader)
	if err != nil {
		return nil, badRequest("create Anthropic batch request", err)
	}
	request.Header.Set("anthropic-version", anthropicapi.SupportedVersion)
	request.Header.Set("accept", "application/json")
	if body != nil {
		request.Header.Set("content-type", "application/json")
	}
	if requestID != "" {
		request.Header.Set("x-request-id", requestID)
	}
	provider.ApplyBedrockProject(request, provider.HeaderBedrockAnthropicWorkspace, adapter.bedrockProjectID)
	if err := adapter.authorizer.Authorize(request, nil); err != nil {
		return nil, &provider.Error{Class: provider.ErrorAuthentication, Message: "authorize Anthropic batch request", Cause: err}
	}
	return request, nil
}

// upstreamBatch is Anthropic's batch object. Its timestamps are RFC 3339
// strings where the northbound shape counts seconds, so they are parsed here
// rather than passed along and misread as zero.
type upstreamBatch struct {
	ID                string             `json:"id"`
	Type              string             `json:"type"`
	ProcessingStatus  string             `json:"processing_status"`
	RequestCounts     batchRequestCounts `json:"request_counts"`
	ResultsURL        string             `json:"results_url"`
	CreatedAt         string             `json:"created_at"`
	EndedAt           string             `json:"ended_at"`
	ExpiresAt         string             `json:"expires_at"`
	ArchivedAt        string             `json:"archived_at"`
	CancelInitiatedAt string             `json:"cancel_initiated_at"`
}

func batchTimestamp(value string) int64 {
	if strings.TrimSpace(value) == "" {
		return 0
	}
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return 0
	}
	return parsed.Unix()
}

// decodeBatchObject renders Anthropic's batch onto the northbound shape.
//
// The upstream identifier is kept as-is; the gateway replaces it with Halro's
// own, the way it does for every other resource. What this cannot fill is
// output_file_id: Anthropic has no file to name, only a results URL, and
// materialising one is the next slice's work.
func decodeBatchObject(raw []byte) (provider.BatchObject, error) {
	var upstream upstreamBatch
	if err := json.Unmarshal(raw, &upstream); err != nil {
		return provider.BatchObject{}, malformed("decode Anthropic batch", err)
	}
	if upstream.ID == "" {
		return provider.BatchObject{}, malformed("Anthropic batch carried no identifier", nil)
	}
	return provider.BatchObject{
		ID: upstream.ID, Object: "batch", Endpoint: "/v1/chat/completions",
		CompletionWindow: "24h",
		Status:           decodeBatchProcessingStatus(upstream.ProcessingStatus, upstream.RequestCounts),
		CreatedAt:        batchTimestamp(upstream.CreatedAt),
		ExpiresAt:        batchTimestamp(upstream.ExpiresAt),
		CompletedAt:      batchTimestamp(upstream.EndedAt),
		CancellingAt:     batchTimestamp(upstream.CancelInitiatedAt),
	}, nil
}

func (adapter *Adapter) CreateBatch(ctx context.Context, call provider.BatchCreateCall) (provider.BatchObject, error) {
	// completion_window is not a knob here. Anthropic expires a batch 24 hours
	// after creation and takes no parameter for it, so accepting another value
	// would report a window the upstream will not honour.
	if window := strings.TrimSpace(call.CompletionWindow); window != "" && window != "24h" {
		return provider.BatchObject{}, badRequest("Anthropic batches expire 24 hours after creation; completion_window must be 24h", nil)
	}
	if call.Endpoint != "" && call.Endpoint != "/v1/chat/completions" {
		return provider.BatchObject{}, badRequest("this batch profile serves /v1/chat/completions", nil)
	}
	payload, err := renderBatchRequests(adapter.profileID, call.ProviderModel, call.InputRequests)
	if err != nil {
		return provider.BatchObject{}, badRequest("prepare Anthropic batch", err)
	}
	raw, err := adapter.doBatch(ctx, http.MethodPost, "", call.RequestID, payload)
	if err != nil {
		return provider.BatchObject{}, err
	}
	return decodeBatchObject(raw)
}

func (adapter *Adapter) GetBatch(ctx context.Context, requestID, id string) (provider.BatchObject, error) {
	raw, err := adapter.doBatch(ctx, http.MethodGet, id, requestID, nil)
	if err != nil {
		return provider.BatchObject{}, err
	}
	return decodeBatchObject(raw)
}

func (adapter *Adapter) CancelBatch(ctx context.Context, requestID, id string) (provider.BatchObject, error) {
	raw, err := adapter.doBatch(ctx, http.MethodPost, id+"/cancel", requestID, nil)
	if err != nil {
		return provider.BatchObject{}, err
	}
	return decodeBatchObject(raw)
}

func (adapter *Adapter) doBatch(ctx context.Context, method, suffix, requestID string, body []byte) ([]byte, error) {
	request, err := adapter.newBatchRequest(ctx, method, suffix, requestID, body)
	if err != nil {
		return nil, err
	}
	response, err := adapter.client.Do(request)
	if err != nil {
		return nil, transportError(err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, decodeHTTPError(response)
	}
	raw, err := readLimited(response.Body, maxResponseBytes)
	if err != nil {
		return nil, malformed("read Anthropic batch response", err)
	}
	return raw, nil
}
