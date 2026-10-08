package metadatajournal

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
)

// Format2 caps collections while decoding, before a valid MAC can be used to
// allocate an arbitrary operation/path slice. The frame itself is already size
// bounded. Domain root codecs have matching key/value/depth limits.
func decodeBoundOps(raw []byte) ([]Op, error) {
	d := json.NewDecoder(bytes.NewReader(raw))
	token, err := d.Token()
	if err != nil || token != json.Delim('[') {
		return nil, errors.New("bound metadata operations are not an array")
	}
	var ops []Op
	for d.More() {
		if len(ops) >= 1024 {
			return nil, errors.New("bound metadata operation count exceeds limit")
		}
		token, err = d.Token()
		if err != nil || token != json.Delim('{') {
			return nil, errors.New("bound metadata operation is not an object")
		}
		var op Op
		var seen uint8
		for d.More() {
			token, err = d.Token()
			if err != nil {
				return nil, err
			}
			field, ok := token.(string)
			if !ok {
				return nil, errors.New("invalid bound metadata operation field")
			}
			var bit uint8
			switch field {
			case "kind":
				bit = 1
			case "path":
				bit = 2
			case "key":
				bit = 4
			case "value":
				bit = 8
			default:
				return nil, errors.New("unknown bound metadata operation field")
			}
			if seen&bit != 0 {
				return nil, errors.New("duplicate bound metadata operation field")
			}
			seen |= bit
			switch field {
			case "kind":
				if err := d.Decode(&op.Kind); err != nil {
					return nil, err
				}
				if len(op.Kind) > 32 {
					return nil, errors.New("bound metadata operation kind exceeds limit")
				}
			case "path":
				token, err = d.Token()
				if err != nil || token != json.Delim('[') {
					return nil, errors.New("bound metadata path is not an array")
				}
				for d.More() {
					if len(op.Path) >= 32 {
						return nil, errors.New("bound metadata path depth exceeds limit")
					}
					var part string
					if err := d.Decode(&part); err != nil {
						return nil, err
					}
					if len(part) == 0 || len(part) > 32768 {
						return nil, errors.New("bound metadata bucket name exceeds limit")
					}
					op.Path = append(op.Path, part)
				}
				if _, err := d.Token(); err != nil {
					return nil, err
				}
			case "key":
				if err := d.Decode(&op.Key); err != nil {
					return nil, err
				}
				if len(op.Key) > 32768 {
					return nil, errors.New("bound metadata key exceeds limit")
				}
			case "value":
				if err := d.Decode(&op.Value); err != nil {
					return nil, err
				}
				if len(op.Value) > 3<<20 {
					return nil, errors.New("bound metadata value exceeds limit")
				}
			}
		}
		if _, err := d.Token(); err != nil {
			return nil, err
		}
		if err := op.validate(); err != nil {
			return nil, err
		}
		ops = append(ops, op)
	}
	if _, err := d.Token(); err != nil {
		return nil, err
	}
	if err := d.Decode(&struct{}{}); err != io.EOF {
		return nil, errors.New("bound metadata operations have trailing JSON")
	}
	if err := validateBoundOps(ops); err != nil {
		return nil, err
	}
	canonical, err := json.Marshal(ops)
	if err != nil || !bytes.Equal(canonical, raw) {
		return nil, errors.New("bound metadata operations are not canonical")
	}
	return ops, nil
}
