package app

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"

	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/replication"
	"github.com/akz142857/Halro/internal/vault"
)

const haGroupClusterKeyBytes = 170 // eight-byte header, 128-byte plaintext, fixed Vault envelope
const haGroupClusterKeyCredential = "system:ha-group-cluster-key"
const haGroupClusterKeyDomain = "halro:ha:group:cluster-key:v1"

var haGroupClusterKeyMagic = [8]byte{'H', 'G', 'C', 'K', 1, 0, 0, 0}

var ErrHAGroupKeyRotationRequired = errors.New("HA master key rotation requires a confirmed whole-group offline operation")

func haGroupClusterKeyPath(cfg config.Config) string {
	return filepath.Join(cfg.Storage.DataDir, "ha-group-cluster.key")
}

func haGroupClusterKeyConfig(cfg config.Config) ([32]byte, error) {
	if cfg.Replication == nil || cfg.Replication.ClusterID == "" || cfg.Replication.NodeID == "" || len(cfg.Replication.ClusterID) > replication.MaxIdentityBytes || len(cfg.Replication.NodeID) > replication.MaxIdentityBytes {
		return [32]byte{}, errors.New("HA cluster-key candidate needs configured cluster and member identities")
	}
	var identities [2 * (replication.MaxIdentityBytes + 2)]byte
	cluster, node := cfg.Replication.ClusterID, cfg.Replication.NodeID
	binary.BigEndian.PutUint16(identities[:2], uint16(len(cluster)))
	copy(identities[2:], cluster)
	start := 2 + len(cluster)
	binary.BigEndian.PutUint16(identities[start:start+2], uint16(len(node)))
	copy(identities[start+2:], node)
	return sha256.Sum256(identities[:start+2+len(node)]), nil
}

// The candidate carries only a stable authentication key across a DEK change.
// It contains no role, term, indexes, root, baseline or readiness permission.
// Creation first authenticates the original actual Member with the supplied
// cluster key; publication belongs to the offline whole-group intent owner.
func encodeHAGroupClusterKeyCandidate(cfg config.Config, clusterKey [32]byte, newMaster []byte) ([]byte, error) {
	configuration, err := haGroupClusterKeyConfig(cfg)
	if err != nil || clusterKey == ([32]byte{}) || len(newMaster) != 32 {
		return nil, errors.Join(errors.New("invalid HA cluster-key candidate"), err)
	}
	actual, err := replication.ReadState(cfg.ReplicationStatePath(), clusterKey[:])
	if err != nil {
		return nil, err
	}
	if actual.ClusterID != cfg.Replication.ClusterID || actual.NodeID != cfg.Replication.NodeID {
		return nil, errors.New("HA cluster-key source Member differs from configured member")
	}
	var plain [128]byte
	defer clear(plain[:])
	copy(plain[:32], clusterKey[:])
	copy(plain[32:64], configuration[:])
	incarnation := sha256.Sum256([]byte(actual.Incarnation))
	copy(plain[64:96], incarnation[:])
	fingerprint := sha256.Sum256(newMaster)
	copy(plain[96:], fingerprint[:])
	v, err := vault.New(newMaster)
	if err != nil {
		return nil, err
	}
	defer v.Close()
	sealed, err := v.EncryptCredential(haGroupClusterKeyCredential, "system", haGroupClusterKeyDomain, plain[:])
	if err != nil {
		return nil, err
	}
	defer clear(sealed)
	raw := make([]byte, haGroupClusterKeyBytes)
	copy(raw, haGroupClusterKeyMagic[:])
	if len(sealed) != len(raw)-len(haGroupClusterKeyMagic) {
		return nil, errors.New("unexpected fixed HA cluster-key envelope size")
	}
	copy(raw[8:], sealed)
	return raw, nil
}

type haGroupClusterKeyReadFailure struct {
	Cause error
	files [2]*os.File
	raw   [haGroupClusterKeyBytes]byte
}

func (e *haGroupClusterKeyReadFailure) Error() string { return e.Cause.Error() }
func (e *haGroupClusterKeyReadFailure) Unwrap() error { return e.Cause }

// This is the fixed, pre-authority key read, analogous to the existing Member
// bootstrap salt read. It never follows a symlink or reads a variable-size
// candidate. Both actual descriptors join before any decrypted key escapes.
func readHAGroupClusterKeyCandidate(cfg config.Config, master []byte) (key, incarnation [32]byte, found bool, resultErr error) {
	return readHAGroupClusterKeyCandidateWithClose(cfg, master, ownedfile.CloseFile)
}

func readHAGroupClusterKeyCandidateWithClose(cfg config.Config, master []byte, closeFD func(*os.File) error) (key, incarnation [32]byte, found bool, resultErr error) {
	path := haGroupClusterKeyPath(cfg)
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return key, incarnation, false, nil
	}
	if err != nil {
		return key, incarnation, false, err
	}
	found = true
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() != haGroupClusterKeyBytes {
		return key, incarnation, found, errors.New("HA cluster-key candidate is not a private fixed-size file")
	}
	configuration, err := haGroupClusterKeyConfig(cfg)
	if err != nil {
		return key, incarnation, found, err
	}
	debt := &haGroupClusterKeyReadFailure{}
	parent, err := os.OpenFile(filepath.Dir(path), os.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return key, incarnation, found, err
	}
	debt.files[0] = parent
	var attempted [2]bool
	defer func() {
		for i, file := range debt.files {
			if file == nil || attempted[i] {
				continue
			}
			attempted[i] = true
			if err := closeFD(file); err != nil {
				debt.Cause = errors.Join(debt.Cause, ownedfile.ErrRetirement, err)
			} else {
				debt.files[i] = nil
			}
		}
		if debt.Cause != nil {
			resultErr = &haGroupClusterKeyReadFailure{Cause: errors.Join(resultErr, debt.Cause), files: debt.files, raw: debt.raw}
		}
		if resultErr != nil {
			clear(key[:])
			clear(incarnation[:])
		}
		if debt.Cause == nil {
			clear(debt.raw[:])
		}
	}()
	parentInfo, err := parent.Stat()
	if err != nil {
		return key, incarnation, found, err
	}
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return key, incarnation, found, err
	}
	debt.files[1] = file
	actual, err := file.Stat()
	if err != nil || !os.SameFile(info, actual) || actual.Size() != haGroupClusterKeyBytes {
		return key, incarnation, found, errors.Join(ownedfile.ErrReplaced, err)
	}
	if _, err := io.ReadFull(file, debt.raw[:]); err != nil {
		return key, incarnation, found, err
	}
	final, statErr := file.Stat()
	named, nameErr := os.Lstat(path)
	namedParent, parentErr := os.Lstat(filepath.Dir(path))
	if statErr != nil || nameErr != nil || parentErr != nil || !os.SameFile(info, final) || !os.SameFile(info, named) || !final.Mode().IsRegular() || !named.Mode().IsRegular() || final.Mode().Perm()&0077 != 0 || named.Mode().Perm()&0077 != 0 || final.Size() != info.Size() || named.Size() != info.Size() || !final.ModTime().Equal(info.ModTime()) || !named.ModTime().Equal(info.ModTime()) || !os.SameFile(parentInfo, namedParent) {
		return key, incarnation, found, errors.Join(ownedfile.ErrReplaced, statErr, nameErr, parentErr)
	}
	// Join the actual readers before constructing even a candidate key.
	for i, f := range debt.files {
		attempted[i] = true
		if err := closeFD(f); err != nil {
			debt.Cause = errors.Join(ownedfile.ErrRetirement, err)
			return key, incarnation, found, debt
		}
		debt.files[i] = nil
	}
	if !bytes.Equal(debt.raw[:8], haGroupClusterKeyMagic[:]) {
		return key, incarnation, found, errors.New("unsupported HA cluster-key candidate")
	}
	v, err := vault.New(master)
	if err != nil {
		return key, incarnation, found, err
	}
	defer v.Close()
	plain, err := v.DecryptCredential(haGroupClusterKeyCredential, "system", haGroupClusterKeyDomain, debt.raw[8:])
	if err != nil {
		return key, incarnation, found, fmt.Errorf("decrypt HA cluster-key candidate: %w", err)
	}
	defer clear(plain)
	fingerprint := sha256.Sum256(master)
	if len(plain) != 128 || !bytes.Equal(plain[32:64], configuration[:]) || !bytes.Equal(plain[96:], fingerprint[:]) {
		return key, incarnation, found, errors.New("HA cluster-key candidate differs from configured member or DEK")
	}
	copy(key[:], plain[:32])
	copy(incarnation[:], plain[64:96])
	if key == ([32]byte{}) {
		return key, incarnation, found, errors.New("empty HA cluster-key candidate")
	}
	return key, incarnation, found, nil
}
