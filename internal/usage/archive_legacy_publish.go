package usage

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/akz142857/Halro/internal/durable"
)

// Transitional offline publisher: preserve schema <=8 startup compatibility
// while streaming the complete legacy root. This does not meet the final
// constant-IO paged-catalog goal. It shares actual write/work ceilings with the
// data partition job and never collects historical Files in memory.
func (e *Exporter) commitLegacyArchive(ctx context.Context, old *archiveManifestInput, missing bool, position ArchivePosition, digest [32]byte, stage *os.File, partitions uint64, cut DiskArchiveCut, writes, work *archiveWork) (err error) {
	return e.commitLegacyArchiveWithProof(ctx, old, missing, position, digest, stage, partitions, cut, writes, work, nil)
}

func (e *Exporter) commitLegacyArchiveWithProof(ctx context.Context, old *archiveManifestInput, missing bool, position ArchivePosition, digest [32]byte, stage *os.File, partitions uint64, cut DiskArchiveCut, writes, work *archiveWork, publication *archiveWindowPublication) (err error) {
	if position.CatalogVersion != 0 || cut.AfterSequence != position.LastSequence || cut.ThroughSequence < position.LastSequence || cut.ThroughSequence > cut.Head.Sequence {
		return ErrDiskUsageCorrupt
	}
	if !missing && position.CompletePrefix && position.SchemaVersion == parquetSchemaVersion && cut.ThroughSequence == position.LastSequence && partitions == 0 {
		if err := old.check(); err != nil {
			return err
		}
		if err := old.file.Sync(); err != nil {
			return err
		}
		return errors.Join(durable.SyncDirectory(e.root), old.check())
	}
	f, err := os.CreateTemp(e.root, ".archive-legacy-*.tmp")
	if err != nil {
		return err
	}
	closed := false
	var identity os.FileInfo
	defer func() {
		if !closed {
			err = errors.Join(err, f.Close())
		}
		named, statErr := os.Lstat(f.Name())
		if errors.Is(statErr, os.ErrNotExist) {
			return // Successful rename, or an already absent owned temporary.
		}
		if statErr != nil || identity == nil || !os.SameFile(identity, named) {
			err = errors.Join(err, statErr, ErrDiskUsageCorrupt)
			return
		}
		err = errors.Join(err, os.Remove(f.Name()))
	}()
	identity, err = f.Stat()
	if err != nil {
		return err
	}
	if err := f.Chmod(0o600); err != nil {
		return err
	}
	hash := sha256.New()
	physical := &archiveWriter{ctx: ctx, out: io.MultiWriter(f, hash), remaining: archiveManifestBytesLimit, work: writes}
	writer := &archiveWriter{ctx: ctx, out: physical, remaining: archiveManifestBytesLimit, work: work}
	if _, err := fmt.Fprintf(writer, `{"schema_version":%d,"last_sequence":%d,"complete_prefix":true,"files":[`, parquetSchemaVersion, cut.ThroughSequence); err != nil {
		return err
	}
	var count uint64
	add := func(entry ManifestFile) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := e.validateCatalogEntry(entry); err != nil {
			return err
		}
		raw, err := json.Marshal(entry)
		if err != nil {
			return err
		}
		if len(raw) > archiveManifestValueLimit || count == ^uint64(0) {
			return ErrDiskUsageLimit
		}
		if count > 0 {
			if _, err := writer.Write([]byte(",")); err != nil {
				return err
			}
		}
		if _, err := writer.Write(raw); err != nil {
			return err
		}
		count++
		return nil
	}
	if !missing {
		if err := work.take(int(old.info.Size())); err != nil {
			return err
		}
		second, secondDigest, err := old.walk(ctx, func(entry ManifestFile) error {
			if entry.MaxSequence > position.LastSequence {
				return ErrDiskUsageCorrupt
			}
			if entry.SchemaVersion == 0 {
				entry.SchemaVersion = position.SchemaVersion
			}
			return add(entry)
		})
		if err != nil || second != position || secondDigest != digest {
			return errors.Join(err, ErrDiskUsageCorrupt)
		}
	}
	if _, err := stage.Seek(0, io.SeekStart); err != nil {
		return err
	}
	info, err := stage.Stat()
	if err != nil {
		return err
	}
	if info.Size() < 0 || info.Size() > archiveManifestBytesLimit {
		return ErrDiskUsageLimit
	}
	if err := work.take(int(info.Size())); err != nil {
		return err
	}
	reader := bufio.NewReaderSize(stage, 32<<10)
	var appended uint64
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		_, err := archiveNonSpace(reader)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		if err := reader.UnreadByte(); err != nil {
			return err
		}
		raw, err := archiveValue(reader)
		if err != nil {
			return err
		}
		var entry ManifestFile
		if err := json.Unmarshal(raw, &entry); err != nil || entry.MinSequence <= position.LastSequence || entry.MaxSequence > cut.ThroughSequence {
			return errors.Join(err, ErrDiskUsageCorrupt)
		}
		if err := add(entry); err != nil {
			return err
		}
		appended++
	}
	if appended != partitions || position.Files > ^uint64(0)-partitions || count != position.Files+partitions {
		return ErrDiskUsageCorrupt
	}
	if _, err := writer.Write([]byte("]}")); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	identity, err = f.Stat()
	if err != nil {
		return err
	}
	err = f.Close()
	closed = true
	if err != nil {
		return err
	}
	if err := checkArchiveTemporary(f, identity); err != nil {
		return err
	}
	if err := publication.checkDirectory(e.root); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if missing {
		if _, err := os.Lstat(filepath.Join(e.root, "manifest.json")); !errors.Is(err, os.ErrNotExist) {
			return errors.Join(err, ErrDiskUsageCorrupt)
		}
	} else if err := old.check(); err != nil {
		return err
	}
	if err := os.Rename(f.Name(), filepath.Join(e.root, "manifest.json")); err != nil {
		return err
	}
	var expectedDigest [32]byte
	copy(expectedDigest[:], hash.Sum(nil))
	expected := ArchivePosition{SchemaVersion: parquetSchemaVersion, LastSequence: cut.ThroughSequence, CompletePrefix: true, Files: count}
	if err := publication.renamed(expected, expectedDigest, identity); err != nil {
		return err
	}
	return durable.SyncDirectory(e.root)
}
