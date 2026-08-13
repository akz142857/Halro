package redaction

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
)

// ErrRewriteRequired reports that the policy would have altered the document.
// It is distinct from a rule rejection because the two mean different things to
// a caller that forwards bytes verbatim: a rejection is "this content is not
// allowed", a rewrite is "this content is allowed only in a form other than the
// one you were about to send". Both unwrap to ErrPolicyRejected so existing
// fail-closed checks keep matching.
var ErrRewriteRequired error = rewriteError{}

type rewriteError struct{}

func (rewriteError) Error() string { return "redaction policy would rewrite content" }
func (rewriteError) Unwrap() error { return ErrPolicyRejected }

// ErrMalformedJSON reports a document the inspector could not parse. It is not
// a policy outcome: nothing was inspected, so nothing may be forwarded.
var ErrMalformedJSON = errors.New("payload is not a single valid JSON document")

// InspectJSON reports whether a policy would alter or reject any part of a JSON
// document, without building a rewritten copy of it.
//
// The native paths forward the caller's bytes verbatim, so "would this change"
// is the only question they can act on — a rewrite is not an option there. Doing
// it as one walk, rather than processing the document and diffing the result
// against a re-marshalled baseline, drops a second full parse and both marshals
// that the diff needed, and removes the class of false positives where the diff
// measured Go's re-rendering (map order, number formatting) rather than the
// policy's decision.
//
// Unlike ProcessJSON this walk covers object member names and numbers as well
// as string values. Those were the two shapes a secret could take and still pass
// — `{"<key is the secret>": 1}` and an unquoted card number — which broke the
// property the native paths depend on: that the inspected surface is the whole
// accepted surface.
func (e *Engine) InspectJSON(policyID, scope string, raw json.RawMessage) error {
	if scope != "inbound" && scope != "outbound" {
		return errors.New("redaction scope is invalid")
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return ErrMalformedJSON
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return ErrMalformedJSON
	}
	var extra any
	if err := decoder.Decode(&extra); err == nil {
		return ErrMalformedJSON
	}
	return e.inspectValue(policyID, scope, value)
}

func (e *Engine) inspectValue(policyID, scope string, value any) error {
	switch typed := value.(type) {
	case string:
		return e.inspectString(policyID, scope, typed)
	case json.Number:
		return e.inspectString(policyID, scope, typed.String())
	case []any:
		for _, element := range typed {
			if err := e.inspectValue(policyID, scope, element); err != nil {
				return err
			}
		}
	case map[string]any:
		for key, element := range typed {
			if err := e.inspectString(policyID, scope, key); err != nil {
				return err
			}
			if err := e.inspectValue(policyID, scope, element); err != nil {
				return err
			}
		}
	}
	return nil
}

func (e *Engine) inspectString(policyID, scope, value string) error {
	processed, err := e.processString(policyID, scope, value)
	if err != nil {
		return err
	}
	if processed != value {
		return ErrRewriteRequired
	}
	return nil
}

// StreamInspector answers the same question as InspectJSON for a stream that is
// forwarded verbatim: it confirms that the outbound baseline leaves the text
// untouched, one fragment at a time, without ever producing a rewritten copy.
//
// It exists because Stream cannot answer that question. Stream withholds the
// trailing max-match-width bytes of every channel so a pattern split across two
// provider chunks cannot be delivered half-redacted; comparing its output
// against the fragment that produced it therefore always differs, which is not
// a policy verdict. The inspector reports how many bytes of the original are
// confirmed unchanged instead, and the caller holds each event until the text it
// carries falls inside that confirmed span.
type StreamInspector struct {
	stream   *Stream
	channels map[string]*inspectChannel
}

type inspectChannel struct {
	rolling *rollingString
	// pending holds the original bytes the rolling window has not yet released,
	// so each released prefix can be compared against exactly the bytes that
	// produced it.
	pending   string
	confirmed int64
	closed    bool
}

func (e *Engine) NewStreamInspector(policyID string) (*StreamInspector, error) {
	stream, err := e.NewStream(policyID)
	if err != nil {
		return nil, err
	}
	return &StreamInspector{stream: stream, channels: make(map[string]*inspectChannel)}, nil
}

// Push feeds the next fragment of one logical text channel — a single content
// block's text, thinking, or tool-argument stream — and returns the number of
// bytes of that channel now confirmed unchanged.
func (i *StreamInspector) Push(channel string, tool bool, piece string) (int64, error) {
	return i.advance(channel, tool, piece, false)
}

// Close finalises one channel, confirming the protected suffix the rolling
// window was still holding back.
func (i *StreamInspector) Close(channel string) (int64, error) {
	state, exists := i.channels[channel]
	if !exists {
		return 0, nil
	}
	if state.closed {
		return state.confirmed, nil
	}
	return i.advance(channel, state.rolling.tool, "", true)
}

// CloseGroup closes every channel whose name carries the given prefix and
// returns the channels it closed. Anthropic ends a content block without naming
// which delta shapes it carried, so the caller closes by block index.
func (i *StreamInspector) CloseGroup(prefix string) ([]string, error) {
	closed := make([]string, 0, len(i.channels))
	for name, state := range i.channels {
		if state.closed || !strings.HasPrefix(name, prefix) {
			continue
		}
		if _, err := i.Close(name); err != nil {
			return closed, err
		}
		closed = append(closed, name)
	}
	return closed, nil
}

// Finish closes every channel still open. A stream that ends without it leaves
// the withheld suffix of each channel uninspected, which is the same leak as not
// inspecting at all.
func (i *StreamInspector) Finish() error {
	for name := range i.channels {
		if _, err := i.Close(name); err != nil {
			return err
		}
	}
	return nil
}

// Confirmed reports the bytes of a channel confirmed unchanged so far.
func (i *StreamInspector) Confirmed(channel string) int64 {
	if state, exists := i.channels[channel]; exists {
		return state.confirmed
	}
	return 0
}

func (i *StreamInspector) advance(channel string, tool bool, piece string, final bool) (int64, error) {
	state, exists := i.channels[channel]
	if !exists {
		state = &inspectChannel{rolling: i.stream.newRolling(tool)}
		i.channels[channel] = state
	}
	if state.closed {
		if piece == "" {
			return state.confirmed, nil
		}
		return state.confirmed, errors.New("stream channel received content after it ended")
	}
	state.pending += piece
	safe, err := state.rolling.Push(piece, final)
	if err != nil {
		return state.confirmed, err
	}
	if len(safe) > len(state.pending) || safe != state.pending[:len(safe)] {
		return state.confirmed, ErrRewriteRequired
	}
	state.pending = state.pending[len(safe):]
	state.confirmed += int64(len(safe))
	if final {
		state.closed = true
		if state.pending != "" {
			return state.confirmed, ErrRewriteRequired
		}
	}
	return state.confirmed, nil
}
