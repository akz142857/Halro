package ledger

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"

	"github.com/akz142857/Halro/internal/ownedfile"
)

// One descriptor is opened for both passes. The captured length never grows;
// digest comparison and identity checks reject replacement/rewrite/pending-roll
// changes, rather than silently mixing two different directory descriptions.
type inspectionManifest struct {
	directory  string
	file       *os.File
	info       os.FileInfo
	parentInfo os.FileInfo
	closed     bool
	closeErr   error
}

var errInspectionManifestStale = errors.New("inspection manifest snapshot is stale")

func openInspectionManifest(directory string) (*inspectionManifest, error) {
	parent, err := os.Lstat(directory)
	if err != nil || !parent.IsDir() {
		return nil, errors.Join(ErrCorrupt, err)
	}
	m := &inspectionManifest{directory: directory, parentInfo: parent}
	named, err := os.Lstat(segmentManifestPath(directory))
	if errors.Is(err, os.ErrNotExist) {
		return m, m.check()
	}
	if err != nil || !named.Mode().IsRegular() {
		return nil, errors.Join(ErrCorrupt, err)
	}
	m.file, err = os.OpenFile(segmentManifestPath(directory), os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	m.info, err = m.file.Stat()
	if err == nil && (!m.info.Mode().IsRegular() || !os.SameFile(named, m.info)) {
		err = ErrCorrupt
	}
	if err == nil {
		err = m.check()
	}
	if err != nil {
		return nil, errors.Join(err, m.close())
	}
	return m, nil
}
func (m *inspectionManifest) walk(consume func(Segment) error) (replicationManifest, [32]byte, error) {
	if m.file == nil {
		return replicationManifest{active: 1}, [32]byte{}, m.check()
	}
	hash := sha256.New()
	result, err := readReplicationManifest(m.directory, io.TeeReader(io.NewSectionReader(m.file, 0, m.info.Size()), hash), consume)
	if err != nil {
		return replicationManifest{}, [32]byte{}, err
	}
	if err := m.check(); err != nil {
		return replicationManifest{}, [32]byte{}, err
	}
	var digest [32]byte
	copy(digest[:], hash.Sum(nil))
	return result, digest, nil
}
func (m *inspectionManifest) check() error {
	if m.closed {
		return errors.Join(ErrCorrupt, errors.New("inspection manifest closed"), m.closeErr)
	}
	parent, parentErr := os.Lstat(m.directory)
	if parentErr != nil || m.parentInfo == nil || !parent.IsDir() || !os.SameFile(m.parentInfo, parent) {
		return errors.Join(ErrCorrupt, errInspectionManifestStale, parentErr)
	}
	named, err := os.Lstat(filepath.Join(m.directory, segmentManifestName))
	if m.file == nil && errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return errors.Join(ErrCorrupt, errInspectionManifestStale, fmt.Errorf("inspection manifest changed: %w", err))
	}
	if m.file == nil {
		return errors.Join(ErrCorrupt, errInspectionManifestStale, errors.New("manifest appeared during inspection"))
	}
	current, err := m.file.Stat()
	if err != nil {
		return err
	}
	if !named.Mode().IsRegular() || !os.SameFile(m.info, named) || !os.SameFile(m.info, current) || m.info.Size() != current.Size() || m.info.Size() != named.Size() || m.info.ModTime() != current.ModTime() || m.info.ModTime() != named.ModTime() {
		return errors.Join(ErrCorrupt, errInspectionManifestStale, errors.New("inspection manifest replaced or rewritten"))
	}
	return nil
}
func (m *inspectionManifest) close() error {
	if m == nil || m.closed {
		if m != nil {
			return m.closeErr
		}
		return nil
	}
	m.closed = true
	if m.file != nil {
		if err := ownedfile.CloseFile(m.file); err != nil {
			m.closeErr = errors.Join(ErrSegmentRetirement, err)
		} else {
			m.file = nil
		}
	}
	return m.closeErr
}
