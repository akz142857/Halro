package metadatajournal

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"

	"github.com/akz142857/Halro/internal/ownedfile"
)

// ConfigureBoundReplicaContext changes only the actual writer's physical mode.
// Its selected caller independently establishes peer/promise/Ordering authority;
// no role or projection permission follows from this native interface. Every
// byte authenticates on the same descriptor before and after mode installation.
func (l *Log) ConfigureBoundReplicaContext(ctx context.Context, a BoundReplicationAuthority, expected Head) error {
	if l == nil || a.Validate() != nil {
		return ErrIncompatibleFormat
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := errors.Join(ctx.Err(), l.availabilityError()); err != nil {
		return err
	}
	actual := Head{Epoch: l.epoch, Sequence: l.sequence, Hash: l.lastHash, Offset: l.offset, TrimmedThrough: l.trimmedThrough, FormatVersion: l.formatVersion, FileRevision: l.fileRevision, Baseline: l.baseline}
	if l.readOnly || l.freezeCancel != nil || l.startupAudit != nil || l.physical != nil || l.beforeDurable != nil || l.afterDurable != nil || l.waitConfirmed != nil || len(l.uncertainAdmissions) != 0 || expected != actual || !a.matches(actual) || l.maxBytes != a.MaxFileBytes || l.boundReplication != nil && *l.boundReplication != a {
		return ErrIncompatibleFormat
	}
	before, err := l.file.Stat()
	named, nameErr := os.Lstat(l.path)
	parent, parentErr := os.Lstat(filepath.Dir(l.path))
	if err != nil || nameErr != nil || parentErr != nil || !before.Mode().IsRegular() || !named.Mode().IsRegular() || !parent.IsDir() || !os.SameFile(before, named) || before.Size() != l.offset {
		return errors.Join(ownedfile.ErrReplaced, err, nameErr, parentErr)
	}
	head, partial, err := scanContext(ctx, io.NewSectionReader(l.file, 0, l.offset), l.key, a.checkControl)
	if err != nil || partial || head != actual {
		return errors.Join(ErrCorrupt, err)
	}
	after, err := l.file.Stat()
	named, nameErr = os.Lstat(l.path)
	parentAfter, parentErr := os.Lstat(filepath.Dir(l.path))
	if err != nil || nameErr != nil || parentErr != nil || !os.SameFile(before, after) || !os.SameFile(before, named) || !os.SameFile(parent, parentAfter) || after.Size() != before.Size() || named.Size() != before.Size() || !after.ModTime().Equal(before.ModTime()) || !named.ModTime().Equal(before.ModTime()) {
		return errors.Join(ownedfile.ErrReplaced, err, nameErr, parentErr)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	l.replica, l.boundReplication = true, &a
	return nil
}
