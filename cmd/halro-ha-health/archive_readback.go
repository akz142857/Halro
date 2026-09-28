package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
)

// archiveReadbackReport proves that two separately read, frozen collector
// copies have the same internally consistent structure and exact file bytes.
// It does not attest that either copy is in an independent immutable store.
type archiveReadbackReport struct {
	Version              int                   `json:"version"`
	Status               string                `json:"status"`
	SourceManifest       string                `json:"source_manifest"`
	ReadbackManifest     string                `json:"readback_manifest"`
	InventorySHA256      string                `json:"inventory_sha256"`
	SourceVerification   durableSnapshotReport `json:"source_verification"`
	ReadbackVerification durableSnapshotReport `json:"readback_verification"`
}

func verifyArchiveReadback(source, readback, environment, cluster string, members []string) (archiveReadbackReport, error) {
	if source == readback {
		return archiveReadbackReport{}, errors.New("readback must be a separate frozen copy")
	}
	sourceReport, err := verifyDurableSnapshot(source, environment, cluster, members)
	if err != nil {
		return archiveReadbackReport{}, fmt.Errorf("verify source snapshot: %w", err)
	}
	readbackReport, err := verifyDurableSnapshot(readback, environment, cluster, members)
	if err != nil {
		return archiveReadbackReport{}, fmt.Errorf("verify readback snapshot: %w", err)
	}
	if !reflect.DeepEqual(sourceReport, readbackReport) {
		return archiveReadbackReport{}, errors.New("readback snapshot differs from source inventory or retained collector history")
	}
	// A hard link would let the same physical files masquerade as a second
	// read. Check every retained segment as well as the current manifest.
	for _, file := range sourceReport.Files {
		sourceInfo, err := os.Stat(filepath.Join(filepath.Dir(source), file.Name))
		if err != nil {
			return archiveReadbackReport{}, err
		}
		readbackInfo, err := os.Stat(filepath.Join(filepath.Dir(readback), file.Name))
		if err != nil {
			return archiveReadbackReport{}, err
		}
		if os.SameFile(sourceInfo, readbackInfo) {
			return archiveReadbackReport{}, fmt.Errorf("readback file %q is the source file or a hard link", file.Name)
		}
	}
	// Detect a change to either copy between the two reads. External freeze and
	// immutable retention must still be established independently.
	sourceAgain, err := verifyDurableSnapshot(source, environment, cluster, members)
	if err != nil || !reflect.DeepEqual(sourceReport, sourceAgain) {
		return archiveReadbackReport{}, errors.New("source snapshot changed during readback verification")
	}
	readbackAgain, err := verifyDurableSnapshot(readback, environment, cluster, members)
	if err != nil || !reflect.DeepEqual(readbackReport, readbackAgain) {
		return archiveReadbackReport{}, errors.New("readback snapshot changed during verification")
	}
	return archiveReadbackReport{Version: 1, Status: "readback_bytes_match_local_only",
		SourceManifest: source, ReadbackManifest: readback, InventorySHA256: sourceReport.InventorySHA256,
		SourceVerification: sourceReport, ReadbackVerification: readbackReport}, nil
}
