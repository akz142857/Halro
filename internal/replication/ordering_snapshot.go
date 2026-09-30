package replication

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
)

// OrderingSnapshotReport is a read-only authenticated inventory of one frozen
// ordering journal. The expected prefix can be shorter than the verified file.
type OrderingSnapshotReport struct {
	Version           int    `json:"version"`
	Status            string `json:"status"`
	ClusterID         string `json:"cluster_id"`
	Incarnation       string `json:"incarnation"`
	ExpectedIndex     uint64 `json:"expected_index"`
	VerifiedLastIndex uint64 `json:"verified_last_index"`
	VerifiedLastTerm  uint64 `json:"verified_last_term"`
	Bytes             uint64 `json:"bytes"`
	FileSHA256        string `json:"file_sha256"`
}

// VerifyOrderingSnapshot never opens its source for writing and rejects an
// incomplete trailing record. It authenticates the whole file and checks the
// exact expected prefix; the caller must freeze the source during the read.
func VerifyOrderingSnapshot(path string, key []byte, clusterID, incarnation string, expectedIndex uint64,
	expectedHead [sha256.Size]byte) (OrderingSnapshotReport, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || len(key) != sha256.Size || clusterID == "" || incarnation == "" ||
		expectedIndex == 0 && expectedHead != ([sha256.Size]byte{}) || expectedIndex > 0 && expectedHead == ([sha256.Size]byte{}) {
		return OrderingSnapshotReport{}, errors.New("ordering snapshot requires an exact path, identity, key and expected prefix")
	}
	parent, err := os.Lstat(filepath.Dir(path))
	if err != nil || !parent.IsDir() || parent.Mode().Perm()&0o022 != 0 {
		return OrderingSnapshotReport{}, errors.New("ordering snapshot parent must be a private directory")
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 || info.Size() == 0 {
		return OrderingSnapshotReport{}, errors.New("ordering snapshot must be a nonempty private regular file")
	}
	file, err := os.Open(path)
	if err != nil {
		return OrderingSnapshotReport{}, err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) || opened.Size() != info.Size() {
		return OrderingSnapshotReport{}, errors.New("ordering snapshot changed before authentication")
	}
	encoded, err := ReadLengthDelimited(file, orderingHeaderFixedBytes+2*MaxIdentityBytes)
	if err != nil {
		return OrderingSnapshotReport{}, err
	}
	header, err := UnmarshalOrderingHeader(encoded, key)
	if err != nil || header.ClusterID != clusterID || header.Incarnation != incarnation {
		return OrderingSnapshotReport{}, errors.New("ordering snapshot header identity or MAC is invalid")
	}
	journal := &OrderingJournal{file: file, header: header, dataOffset: int64(len(encoded)), nextOffset: int64(len(encoded))}
	copy(journal.key[:], key)
	defer clear(journal.key[:])
	if err := journal.recover(expectedIndex, expectedHead, false); err != nil {
		return OrderingSnapshotReport{}, err
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return OrderingSnapshotReport{}, err
	}
	root := sha256.New()
	n, err := io.Copy(root, file)
	if err != nil || n != info.Size() {
		return OrderingSnapshotReport{}, errors.New("ordering snapshot changed during inventory")
	}
	final, err := os.Lstat(path)
	if err != nil || !os.SameFile(info, final) || final.Size() != info.Size() {
		return OrderingSnapshotReport{}, errors.New("ordering snapshot changed after authentication")
	}
	return OrderingSnapshotReport{Version: 1, Status: "ordering_mac_verified", ClusterID: clusterID,
		Incarnation: incarnation, ExpectedIndex: expectedIndex, VerifiedLastIndex: journal.lastIndex,
		VerifiedLastTerm: journal.lastTerm, Bytes: uint64(n), FileSHA256: hex.EncodeToString(root.Sum(nil))}, nil
}
