package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"syscall"

	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/dirscan"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

// Legacy format 1 carries its whole file declaration in one JSON object. Its
// finite structural limits refuse larger declarations; they are not a native
// data-capacity promise or a paged/streaming seed-format migration.
const seedManifestBytes = 8 << 20
const seedManifestFiles = 4096
const seedTreeDepth = 32
const seedWorkRAMBytes int64 = 64 << 20

var errSeedLimit = errors.New("legacy seed declaration exceeds its bounded format limit")
var errBoundedFile = errors.New("regular source file exceeds its size or identity bound")
var errBoundedFileRetirement = errors.New("bounded source descriptor retirement is unproven")

func seedDeclarationBound(manifest SeedManifest) error {
	if manifest.Selected != nil {
		q := manifest.Selected
		if manifest.Version != selectedSeedManifestVersion || len(manifest.Files) != 0 || len(q.SourceDataDir) > 4096 || len(q.SourceMetadataFile) > 4096 || q.SourceMember.Validate() != nil {
			return errSeedLimit
		}
		for _, p := range q.SourceMember.Peers {
			if len(p.Address) > 4096 {
				return errSeedLimit
			}
		}
	}
	if len(manifest.Files) > seedManifestFiles {
		return errSeedLimit
	}
	// Covers worst JSON escaping, indentation/numeric fields, and the fixed
	// scalar envelope before Marshal allocates its output. No history graph is
	// traversed beyond the fixed maximum declaration count.
	remaining := seedManifestBytes - 4096 - 256*len(manifest.Files)
	take := func(value string) bool {
		if len(value) > remaining/6 {
			return false
		}
		remaining -= 6 * len(value)
		return true
	}
	for _, scalar := range [7]string{manifest.ClusterID, manifest.Incarnation, manifest.SourceNode, manifest.TargetNode, manifest.OrderingHeadMAC, manifest.ApprovedBy, manifest.MAC} {
		if !take(scalar) {
			return errSeedLimit
		}
	}
	for _, file := range manifest.Files {
		if len(file.Path) > 4096 || !take(file.Path) || !take(file.SHA256) {
			return errSeedLimit
		}
	}
	return nil
}

func seedCommandContext(ctx context.Context, cfg config.Config) (context.Context, resourcebudget.Permit, error) {
	ctx, pressure, err := seedBudgetContext(ctx, cfg)
	if err != nil || cfg.ResourceBudget == nil {
		return ctx, resourcebudget.Permit{}, err
	}
	permit, err := pressure.logicalBudget().TryAcquireRecovery(resourcebudget.Cost{RAM: seedWorkRAMBytes})
	return ctx, permit, err
}

func readSeedManifest(ctx context.Context, path string) (payload []byte, resultErr error) {
	payload, resultErr = readBoundedRegularFile(ctx, path, seedManifestBytes)
	if errors.Is(resultErr, errBoundedFile) {
		resultErr = errors.Join(errSeedLimit, resultErr)
	}
	return payload, resultErr
}

func readBoundedRegularFile(ctx context.Context, path string, maximumBytes int64) (payload []byte, resultErr error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if maximumBytes <= 0 || !info.Mode().IsRegular() || info.Size() > maximumBytes {
		return nil, errBoundedFile
	}
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err := file.Close(); err != nil {
			resultErr = errors.Join(resultErr, errBoundedFileRetirement, err)
		}
		resultErr = errors.Join(resultErr, ctx.Err())
		if resultErr != nil {
			payload = nil
		}
	}()
	actual, err := file.Stat()
	if err != nil || !os.SameFile(info, actual) || !actual.Mode().IsRegular() || actual.Size() > maximumBytes {
		return nil, errors.Join(errBoundedFile, err)
	}
	payload, err = io.ReadAll(io.LimitReader(seedContextReader{ctx, file}, maximumBytes+1))
	if err != nil || int64(len(payload)) > maximumBytes {
		return nil, errors.Join(errBoundedFile, err)
	}
	named, err := os.Lstat(path)
	end, statErr := file.Stat()
	if err != nil || statErr != nil || !os.SameFile(info, named) || !info.ModTime().Equal(named.ModTime()) || info.Size() != named.Size() || !os.SameFile(actual, end) || !actual.ModTime().Equal(end.ModTime()) || actual.Size() != end.Size() {
		return nil, errors.Join(dirscan.ErrIdentity, err, statErr)
	}
	return payload, nil
}

type seedContextReader struct {
	ctx context.Context
	io.Reader
}

func (r seedContextReader) Read(buffer []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.Reader.Read(buffer)
}

// Tokenize the files array ourselves. A byte-limited `{}` array can otherwise
// allocate millions of empty SeedFile structs before a post-decode count check.
func decodeSeedManifest(payload []byte) (SeedManifest, error) {
	var manifest SeedManifest
	if len(payload) > seedManifestBytes {
		return manifest, errSeedLimit
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	start, err := decoder.Token()
	if err != nil || start != json.Delim('{') {
		return SeedManifest{}, errors.Join(err, errors.New("seed manifest must be an object"))
	}
	var seen uint16
	decode := func(bit uint16, target any) error {
		if seen&bit != 0 {
			return errors.New("seed manifest contains a duplicate field")
		}
		seen |= bit
		return decoder.Decode(target)
	}
	for decoder.More() {
		name, err := decoder.Token()
		if err != nil {
			return SeedManifest{}, err
		}
		switch name {
		case "version":
			err = decode(1<<0, &manifest.Version)
		case "cluster_id":
			err = decode(1<<1, &manifest.ClusterID)
		case "incarnation":
			err = decode(1<<2, &manifest.Incarnation)
		case "source_node":
			err = decode(1<<3, &manifest.SourceNode)
		case "target_node":
			err = decode(1<<4, &manifest.TargetNode)
		case "term":
			err = decode(1<<5, &manifest.Term)
		case "index":
			err = decode(1<<6, &manifest.Index)
		case "ordering_head_mac":
			err = decode(1<<7, &manifest.OrderingHeadMAC)
		case "projection":
			err = decode(1<<8, &manifest.Projection)
		case "approved_by":
			err = decode(1<<9, &manifest.ApprovedBy)
		case "approved_at":
			err = decode(1<<10, &manifest.ApprovedAt)
		case "files":
			if seen&(1<<11) != 0 {
				return SeedManifest{}, errors.New("seed manifest contains duplicate files")
			}
			seen |= 1 << 11
			array, tokenErr := decoder.Token()
			if tokenErr != nil || array != json.Delim('[') {
				return SeedManifest{}, errors.Join(tokenErr, errors.New("seed files must be an array"))
			}
			for decoder.More() {
				if len(manifest.Files) >= seedManifestFiles {
					return SeedManifest{}, errSeedLimit
				}
				var file SeedFile
				if err := decoder.Decode(&file); err != nil {
					return SeedManifest{}, err
				}
				manifest.Files = append(manifest.Files, file)
			}
			end, tokenErr := decoder.Token()
			if tokenErr != nil || end != json.Delim(']') {
				return SeedManifest{}, errors.Join(tokenErr, errors.New("seed files array is incomplete"))
			}
		case "mac":
			err = decode(1<<12, &manifest.MAC)
		case "selected_state":
			err = decode(1<<13, &manifest.Selected)
		default:
			return SeedManifest{}, errors.New("seed manifest contains an unknown field")
		}
		if err != nil {
			return SeedManifest{}, err
		}
	}
	end, err := decoder.Token()
	if err != nil || end != json.Delim('}') {
		return SeedManifest{}, errors.Join(err, errors.New("seed manifest is incomplete"))
	}
	if err := requireJSONEOF(decoder); err != nil {
		return SeedManifest{}, err
	}
	if err := seedDeclarationBound(manifest); err != nil {
		return SeedManifest{}, err
	}
	return manifest, nil
}
