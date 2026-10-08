package app

import (
	"bytes"
	"encoding/json"
	"errors"
	"github.com/akz142857/Halro/internal/replication"
)

const selectedSeedDeclarationBytes = 1 << 20

// The selected declaration has a fixed root vector and at most two peers.
// Tokenizing its only variable array enforces the count before allocation.
func (s *selectedSeedState) UnmarshalJSON(payload []byte) error {
	if len(payload) > selectedSeedDeclarationBytes {
		return errSeedLimit
	}
	var next selectedSeedState
	d := json.NewDecoder(bytes.NewReader(payload))
	d.DisallowUnknownFields()
	start, err := d.Token()
	if err != nil || start != json.Delim('{') {
		return errors.New("selected seed declaration must be an object")
	}
	var seen uint8
	for d.More() {
		token, err := d.Token()
		if err != nil {
			return err
		}
		name, ok := token.(string)
		if !ok {
			return errSeedLimit
		}
		var bit uint8
		var dst any
		switch name {
		case "source_data_dir":
			bit = 1
			dst = &next.SourceDataDir
		case "source_metadata_file":
			bit = 2
			dst = &next.SourceMetadataFile
		case "source_member":
			bit = 4
		case "source_seal":
			bit = 8
			dst = &next.SourceSeal
		case "metadata_head":
			bit = 16
			dst = &next.MetadataHead
		case "baseline_digest":
			bit = 32
			dst = &next.BaselineDigest
		case "tree":
			bit = 64
			dst = &next.Tree
		default:
			return errors.New("selected seed declaration contains an unknown field")
		}
		if seen&bit != 0 {
			return errors.New("selected seed declaration contains a duplicate field")
		}
		seen |= bit
		if bit == 4 {
			var raw json.RawMessage
			if err = d.Decode(&raw); err != nil {
				return err
			}
			if next.SourceMember, err = decodeSelectedSeedMember(raw); err != nil {
				return err
			}
		} else if err = d.Decode(dst); err != nil {
			return err
		}
	}
	end, err := d.Token()
	if err != nil || end != json.Delim('}') || seen != 127 {
		return errors.New("selected seed declaration is incomplete")
	}
	if err = requireJSONEOF(d); err != nil {
		return err
	}
	if len(next.SourceDataDir) > 4096 || len(next.SourceMetadataFile) > 4096 {
		return errSeedLimit
	}
	*s = next
	return nil
}

func decodeSelectedSeedMember(payload []byte) (replication.MemberState, error) {
	var s replication.MemberState
	d := json.NewDecoder(bytes.NewReader(payload))
	d.DisallowUnknownFields()
	start, err := d.Token()
	if err != nil || start != json.Delim('{') {
		return s, errSeedLimit
	}
	var seen uint16
	for d.More() {
		token, err := d.Token()
		if err != nil {
			return s, err
		}
		var bit uint16
		var dst any
		switch token {
		case "Version":
			bit = 1 << 0
			dst = &s.Version
		case "ClusterID":
			bit = 1 << 1
			dst = &s.ClusterID
		case "Incarnation":
			bit = 1 << 2
			dst = &s.Incarnation
		case "NodeID":
			bit = 1 << 3
			dst = &s.NodeID
		case "Role":
			bit = 1 << 4
			dst = &s.Role
		case "Term":
			bit = 1 << 5
			dst = &s.Term
		case "PromisedTerm":
			bit = 1 << 6
			dst = &s.PromisedTerm
		case "DurableIndex":
			bit = 1 << 7
			dst = &s.DurableIndex
		case "ConfirmedIndex":
			bit = 1 << 8
			dst = &s.ConfirmedIndex
		case "AppliedIndex":
			bit = 1 << 9
			dst = &s.AppliedIndex
		case "OrderingHeadMAC":
			bit = 1 << 10
			dst = &s.OrderingHeadMAC
		case "Projection":
			bit = 1 << 11
			dst = &s.Projection
		case "Peers":
			bit = 1 << 12
		default:
			return s, errors.New("selected seed member contains an unknown field")
		}
		if seen&bit != 0 {
			return s, errors.New("selected seed member contains a duplicate field")
		}
		seen |= bit
		if bit == 1<<12 {
			start, err = d.Token()
			if err != nil || start != json.Delim('[') {
				return s, errSeedLimit
			}
			for d.More() {
				if len(s.Peers) >= 2 {
					return s, errSeedLimit
				}
				var peer replication.StatePeer
				if err = d.Decode(&peer); err != nil {
					return s, err
				}
				if len(peer.Address) > 4096 {
					return s, errSeedLimit
				}
				s.Peers = append(s.Peers, peer)
			}
			end, err := d.Token()
			if err != nil || end != json.Delim(']') {
				return s, errSeedLimit
			}
		} else if err = d.Decode(dst); err != nil {
			return s, err
		}
	}
	end, err := d.Token()
	if err != nil || end != json.Delim('}') || seen != (1<<13)-1 {
		return s, errSeedLimit
	}
	if err = requireJSONEOF(d); err != nil {
		return s, err
	}
	return s, s.Validate()
}
