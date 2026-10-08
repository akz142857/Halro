package ledger

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
)

// One descriptor has a fixed schema; the history is the number of descriptors,
// not the size of an individual JSON value. Refuse oversized values before
// json.Unmarshal can allocate their strings or unknown fields.
const replicationManifestValueLimit = 64 << 10

type replicationManifest struct {
	segment Segment
	count   uint64
	sealed  bool
	active  uint64
	pending bool
}

// findReplicationGeneration reads and validates the complete manifest without
// collecting its segments. It keeps only the requested descriptor and pending
// intent. Other manifest/replay APIs still require their separate migration.
func findReplicationGeneration(directory string, generation uint64) (replicationManifest, error) {
	var selected Segment
	found := false
	result, err := walkReplicationManifest(directory, func(segment Segment) error {
		if segment.Generation == generation {
			selected, found = segment, true
		}
		return nil
	})
	if err != nil {
		return replicationManifest{}, err
	}
	if !found && generation != result.active {
		return replicationManifest{}, errors.New("ledger generation is not present")
	}
	result.segment, result.sealed = selected, found
	return result, nil
}

func walkReplicationManifest(directory string, consume func(Segment) error) (replicationManifest, error) {
	file, err := os.Open(segmentManifestPath(directory))
	if errors.Is(err, os.ErrNotExist) {
		return replicationManifest{active: 1}, nil
	}
	if err != nil {
		return replicationManifest{}, err
	}
	defer file.Close()
	return readReplicationManifest(directory, file, consume)
}

// Reads one capped descriptor at a time. Callbacks are tentative until the
// complete parser/pending-resolution succeeds; never publish during this call.
func readReplicationManifest(directory string, source io.Reader, consume func(Segment) error) (replicationManifest, error) {
	parsed, err := parseSegmentManifest(source, consume)
	if err != nil {
		return replicationManifest{}, err
	}
	result := replicationManifest{segment: parsed.last, count: parsed.count, sealed: parsed.count > 0, active: parsed.count + 1, pending: parsed.pending != nil}
	if pending := parsed.pending; pending != nil {
		checksum, length, err := hashFile(filepath.Join(directory, pending.File))
		if !errors.Is(err, os.ErrNotExist) {
			if err != nil {
				return replicationManifest{}, err
			}
			if length != pending.Length || checksum != pending.PlainChecksum {
				return replicationManifest{}, fmt.Errorf("%w: interrupted roll generation is unreadable", ErrCorrupt)
			}
			result.segment, result.sealed = *pending, true
			result.count++
			if consume != nil {
				if err := consume(*pending); err != nil {
					return replicationManifest{}, err
				}
			}
			result.active++
			if result.active == 0 {
				return replicationManifest{}, fmt.Errorf("%w: generation overflow", ErrCorrupt)
			}
		}
	}
	return result, nil
}

type parsedSegmentManifest struct {
	version int
	count   uint64
	last    Segment
	pending *Segment
	array   bool
}

// parseSegmentManifest bounds each JSON value and validates the complete old
// schema before returning its pending intent. Callbacks remain tentative until
// parsing succeeds; this parser does not resolve or mutate filesystem state.
func parseSegmentManifest(source io.Reader, consume func(Segment) error) (parsedSegmentManifest, error) {
	return parseSegmentManifestLocations(source, func(segment Segment, _ int64, _ []byte) error {
		if consume != nil {
			return consume(segment)
		}
		return nil
	})
}

type manifestCountingReader struct {
	source io.Reader
	offset int64
}

func (r *manifestCountingReader) Read(p []byte) (int, error) {
	n, err := r.source.Read(p)
	if n < 0 || n > len(p) || r.offset < 0 || int64(n) > math.MaxInt64-r.offset {
		return 0, fmt.Errorf("%w: manifest read position overflow", ErrCorrupt)
	}
	r.offset += int64(n)
	return n, err
}

// The observer receives exact raw-value positions in the same opened source.
// Like the legacy consumer, it owns only tentative output until EOF validates.
func parseSegmentManifestLocations(source io.Reader, consume func(Segment, int64, []byte) error) (parsedSegmentManifest, error) {
	counted := &manifestCountingReader{source: source}
	input := bufio.NewReaderSize(counted, replicationManifestValueLimit)
	if err := manifestExpect(input, '{'); err != nil {
		return parsedSegmentManifest{}, err
	}
	var result parsedSegmentManifest
	var count uint64
	version := 0
	var pending *Segment
	seenVersion, seenSegments, seenPending := false, false, false
	for {
		c, err := manifestNonSpace(input)
		if err != nil {
			return parsedSegmentManifest{}, err
		}
		if c == '}' {
			break
		}
		if err := input.UnreadByte(); err != nil {
			return parsedSegmentManifest{}, err
		}
		raw, err := manifestValue(input)
		if err != nil {
			return parsedSegmentManifest{}, err
		}
		var name string
		if len(raw) == 0 || raw[0] != '"' || json.Unmarshal(raw, &name) != nil {
			return parsedSegmentManifest{}, fmt.Errorf("%w: invalid manifest field", ErrCorrupt)
		}
		if err := manifestExpect(input, ':'); err != nil {
			return parsedSegmentManifest{}, err
		}
		switch name {
		case "segments":
			if seenSegments {
				return parsedSegmentManifest{}, fmt.Errorf("%w: duplicate segments field", ErrCorrupt)
			}
			seenSegments = true
			c, err := manifestNonSpace(input)
			if err != nil {
				return parsedSegmentManifest{}, err
			}
			if c == 'n' {
				if err := input.UnreadByte(); err != nil {
					return parsedSegmentManifest{}, err
				}
				raw, err := manifestValue(input)
				if err != nil || string(raw) != "null" {
					return parsedSegmentManifest{}, fmt.Errorf("%w: invalid segments field", ErrCorrupt)
				}
			} else if c == '[' {
				result.array = true
				for {
					c, err := manifestNonSpace(input)
					if err != nil {
						return parsedSegmentManifest{}, err
					}
					if c == ']' {
						break
					}
					if err := input.UnreadByte(); err != nil {
						return parsedSegmentManifest{}, err
					}
					raw, err := manifestValue(input)
					if err != nil {
						return parsedSegmentManifest{}, err
					}
					var segment Segment
					if err := json.Unmarshal(raw, &segment); err != nil {
						return parsedSegmentManifest{}, fmt.Errorf("%w: invalid segment descriptor", ErrCorrupt)
					}
					count++
					if count == 0 || segment.Generation != count {
						return parsedSegmentManifest{}, fmt.Errorf("%w: sealed generations must run from 1 without gaps", ErrCorrupt)
					}
					result.last = segment
					if consume != nil {
						position := counted.offset - int64(input.Buffered()) - int64(len(raw))
						if position < 0 {
							return parsedSegmentManifest{}, fmt.Errorf("%w: invalid descriptor position", ErrCorrupt)
						}
						if err := consume(segment, position, raw); err != nil {
							return parsedSegmentManifest{}, err
						}
					}
					c, err = manifestNonSpace(input)
					if err != nil {
						return parsedSegmentManifest{}, err
					}
					if c == ']' {
						break
					}
					if c != ',' {
						return parsedSegmentManifest{}, fmt.Errorf("%w: invalid segments separator", ErrCorrupt)
					}
					// A comma requires a descriptor; do not accept trailing commas.
					c, err = manifestNonSpace(input)
					if err != nil || c == ']' {
						return parsedSegmentManifest{}, fmt.Errorf("%w: missing segment descriptor", ErrCorrupt)
					}
					if err := input.UnreadByte(); err != nil {
						return parsedSegmentManifest{}, err
					}
				}
			} else {
				return parsedSegmentManifest{}, fmt.Errorf("%w: segments must be an array or null", ErrCorrupt)
			}
		default:
			raw, err := manifestValue(input)
			if err != nil {
				return parsedSegmentManifest{}, err
			}
			if !json.Valid(raw) {
				return parsedSegmentManifest{}, fmt.Errorf("%w: invalid manifest value", ErrCorrupt)
			}
			switch name {
			case "version":
				if seenVersion || json.Unmarshal(raw, &version) != nil {
					return parsedSegmentManifest{}, fmt.Errorf("%w: invalid or duplicate manifest version", ErrCorrupt)
				}
				seenVersion = true
			case "pending":
				if seenPending || json.Unmarshal(raw, &pending) != nil {
					return parsedSegmentManifest{}, fmt.Errorf("%w: invalid or duplicate roll intent", ErrCorrupt)
				}
				seenPending = true
			}
		}
		c, err = manifestNonSpace(input)
		if err != nil {
			return parsedSegmentManifest{}, err
		}
		if c == '}' {
			break
		}
		if c != ',' {
			return parsedSegmentManifest{}, fmt.Errorf("%w: invalid manifest separator", ErrCorrupt)
		}
		c, err = manifestNonSpace(input)
		if err != nil || c == '}' {
			return parsedSegmentManifest{}, fmt.Errorf("%w: missing manifest field", ErrCorrupt)
		}
		if err := input.UnreadByte(); err != nil {
			return parsedSegmentManifest{}, err
		}
	}
	if _, err := manifestNonSpace(input); !errors.Is(err, io.EOF) {
		return parsedSegmentManifest{}, fmt.Errorf("%w: trailing manifest bytes", ErrCorrupt)
	}
	if version < 1 || version > segmentManifestVersion {
		return parsedSegmentManifest{}, fmt.Errorf("%w: ledger segment manifest version %d", ErrUnsupportedVersion, version)
	}
	if count+1 == 0 {
		return parsedSegmentManifest{}, fmt.Errorf("%w: generation overflow", ErrCorrupt)
	}
	if pending != nil && pending.Generation != count+1 {
		return parsedSegmentManifest{}, fmt.Errorf("%w: pending generation does not continue sealed history", ErrCorrupt)
	}
	result.version, result.count, result.pending = version, count, pending
	return result, nil
}

func manifestNonSpace(input *bufio.Reader) (byte, error) {
	for {
		c, err := input.ReadByte()
		if err != nil {
			return 0, err
		}
		if c != ' ' && c != '\n' && c != '\r' && c != '\t' {
			return c, nil
		}
	}
}

func manifestExpect(input *bufio.Reader, want byte) error {
	c, err := manifestNonSpace(input)
	if err != nil {
		return fmt.Errorf("%w: incomplete manifest: %v", ErrCorrupt, err)
	}
	if c != want {
		return fmt.Errorf("%w: expected manifest delimiter %q", ErrCorrupt, want)
	}
	return nil
}

// manifestValue bounds a JSON value before decoding. It deliberately handles
// only framing; json.Unmarshal/Valid still performs JSON grammar validation.
func manifestValue(input *bufio.Reader) ([]byte, error) {
	c, err := manifestNonSpace(input)
	if err != nil {
		return nil, err
	}
	value := []byte{c}
	depth := 0
	inString, escaped := c == '"', false
	if c == '{' || c == '[' {
		depth = 1
	}
	structured := inString || depth > 0
	for {
		if !structured {
			next, err := input.Peek(1)
			if errors.Is(err, io.EOF) {
				return value, nil
			}
			if err != nil {
				return nil, err
			}
			n := next[0]
			if n == ',' || n == '}' || n == ']' || n == ' ' || n == '\n' || n == '\r' || n == '\t' {
				return value, nil
			}
		}
		if len(value) >= replicationManifestValueLimit {
			return nil, fmt.Errorf("%w: manifest value exceeds %d bytes", ErrCorrupt, replicationManifestValueLimit)
		}
		c, err := input.ReadByte()
		if err != nil {
			return nil, fmt.Errorf("%w: incomplete manifest value", ErrCorrupt)
		}
		value = append(value, c)
		if inString {
			if escaped {
				escaped = false
			} else if c == '\\' {
				escaped = true
			} else if c == '"' {
				inString = false
				if depth == 0 {
					return value, nil
				}
			}
		} else {
			switch c {
			case '"':
				inString = true
			case '{', '[':
				depth++
			case '}', ']':
				depth--
				if depth == 0 {
					return value, nil
				}
			}
		}
	}
}
