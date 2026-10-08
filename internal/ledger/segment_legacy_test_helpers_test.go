package ledger

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/akz142857/Halro/internal/durable"
)

type segmentManifest struct {
	Version  int       `json:"version"`
	Segments []Segment `json:"segments"`
	Pending  *Segment  `json:"pending,omitempty"`
}

func segmentManifestSliceSource(manifest segmentManifest) segmentManifestSource {
	return segmentManifestSource{nilSegments: manifest.Segments == nil, pending: manifest.Pending, each: func(emit func(Segment) error) error {
		for _, segment := range manifest.Segments {
			if err := emit(segment); err != nil {
				return err
			}
		}
		return nil
	}}
}

func writeSegmentManifest(output io.Writer, manifest segmentManifest) error {
	return writeSegmentManifestSource(output, segmentManifestSliceSource(manifest))
}

func saveSegmentManifestWithSync(directory string, manifest segmentManifest, syncDirectory func(string) error) (bool, error) {
	return saveSegmentManifestSourceWithSync(directory, segmentManifestSliceSource(manifest), syncDirectory)
}

// These collectors exist only as small-fixture oracles. Production inspection
// and replication paths use the streaming manifest reader.
func loadSegmentManifest(directory string) (segmentManifest, error) {
	file, err := os.Open(segmentManifestPath(directory))
	if errors.Is(err, os.ErrNotExist) {
		return segmentManifest{Version: segmentManifestVersion}, nil
	}
	if err != nil {
		return segmentManifest{}, fmt.Errorf("read ledger segment manifest: %w", err)
	}
	defer file.Close()
	var manifest segmentManifest
	parsed, err := parseSegmentManifest(file, func(segment Segment) error {
		manifest.Segments = append(manifest.Segments, segment)
		return nil
	})
	if err != nil {
		return segmentManifest{}, err
	}
	manifest.Version, manifest.Pending = parsed.version, parsed.pending
	if parsed.array && manifest.Segments == nil {
		manifest.Segments = []Segment{}
	}
	return manifest, nil
}

func saveSegmentManifest(directory string, manifest segmentManifest) error {
	_, err := saveSegmentManifestWithSync(directory, manifest, durable.SyncDirectory)
	return err
}

func resolveSegments(directory string) ([]Segment, bool, error) {
	manifest, err := loadSegmentManifest(directory)
	if err != nil {
		return nil, false, err
	}
	if manifest.Pending == nil {
		return manifest.Segments, false, nil
	}
	pending := *manifest.Pending
	checksum, length, err := hashFile(filepath.Join(directory, pending.File))
	if errors.Is(err, os.ErrNotExist) {
		return manifest.Segments, true, nil
	}
	if err != nil {
		return nil, false, err
	}
	if length != pending.Length || checksum != pending.PlainChecksum {
		return nil, false, fmt.Errorf("%w: interrupted ledger roll left generation %d unreadable",
			ErrCorrupt, pending.Generation)
	}
	return append(manifest.Segments, pending), true, nil
}

func activeGeneration(segments []Segment) uint64 {
	if len(segments) == 0 {
		return 1
	}
	return segments[len(segments)-1].Generation + 1
}
