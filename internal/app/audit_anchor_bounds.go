package app

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"syscall"

	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/resourcebudget"
	boltstore "github.com/akz142857/Halro/internal/store/bolt"
)

// The legacy anchor-array API has an explicit input/output graph bound. Larger
// witness histories require a future paged interface, not silent truncation.
const anchorFileBytes = 8 << 20
const anchorLineBytes = 64 << 10
const anchorMaximumRows = 4096
const anchorWorkRAMBytes int64 = 64 << 20

var errAnchorLimit = errors.New("audit witness exceeds bounded array or file format limit")
var errAnchorRetirement = errors.New("audit witness descriptor retirement is unproven")

func boundedAnchor(anchor boltstore.AuditAnchor) bool {
	for _, value := range [5]string{anchor.ClusterID, anchor.Incarnation, anchor.InstanceID, anchor.NodeID, anchor.TargetID} {
		if len(value) > 256 {
			return false
		}
	}
	return true
}

func readAnchorLinesContext(ctx context.Context, path string, maximumRows int) (anchors []boltstore.AuditAnchor, resultErr error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > anchorFileBytes {
		return nil, errAnchorLimit
	}
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err := file.Close(); err != nil {
			resultErr = errors.Join(resultErr, errAnchorRetirement, err)
		}
		resultErr = errors.Join(resultErr, ctx.Err())
		if resultErr != nil {
			anchors = nil
		}
	}()
	actual, err := file.Stat()
	if err != nil || !os.SameFile(info, actual) || actual.Size() != info.Size() || !actual.ModTime().Equal(info.ModTime()) {
		return nil, errors.Join(errAnchorLimit, err)
	}
	reader := bufio.NewReaderSize(io.NewSectionReader(file, 0, actual.Size()), anchorLineBytes)
	for lineNumber := 1; ; lineNumber++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		raw, readErr := reader.ReadSlice('\n')
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return nil, errors.Join(errAnchorLimit, readErr)
		}
		line := bytes.TrimSpace(raw)
		if len(line) > 0 {
			if len(anchors) >= maximumRows {
				return nil, errAnchorLimit
			}
			var anchor boltstore.AuditAnchor
			if err := json.Unmarshal(line, &anchor); err != nil {
				// Only an unterminated malformed final line can be a torn
				// witness write. Intact following lines make it corruption.
				if !errors.Is(readErr, io.EOF) {
					return nil, fmt.Errorf("decode anchor line %d: %w", lineNumber, err)
				}
			} else {
				if !boundedAnchor(anchor) {
					return nil, errAnchorLimit
				}
				anchors = append(anchors, anchor)
			}
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
	}
	named, nameErr := os.Lstat(path)
	end, statErr := file.Stat()
	if nameErr != nil || statErr != nil || !os.SameFile(actual, named) || !os.SameFile(actual, end) || actual.Size() != end.Size() || !actual.ModTime().Equal(end.ModTime()) || actual.Size() != named.Size() || !actual.ModTime().Equal(named.ModTime()) {
		return nil, errors.Join(errAnchorLimit, nameErr, statErr)
	}
	return anchors, nil
}

type anchorWorkKey struct{}

// Only this synchronous stack creates a work marker. Nested native reads and
// result encoding keep the same original command Manager and whole-work fee.
func anchorCommandContext(ctx context.Context, cfg config.Config) (context.Context, func() error, error) {
	ctx, pressure, err := commandBudgetContext(ctx, cfg)
	if err != nil {
		return ctx, nil, err
	}
	manager := pressure.logicalBudget()
	if original, ok := ctx.Value(anchorWorkKey{}).(*resourcebudget.Manager); ok {
		if original != manager {
			return ctx, nil, resourcebudget.ErrInvalid
		}
		return ctx, func() error { return nil }, nil
	}
	var permit resourcebudget.Permit
	if cfg.ResourceBudget != nil {
		permit, err = manager.TryAcquireRecovery(resourcebudget.Cost{RAM: anchorWorkRAMBytes})
		if err != nil {
			return ctx, nil, err
		}
	}
	return context.WithValue(ctx, anchorWorkKey{}, manager), func() error {
		if permit == (resourcebudget.Permit{}) {
			return nil
		}
		return permit.Release()
	}, nil
}

// WriteVerifyAuditAnchorsFile holds input, exact conflict maps, verdicts and
// JSON encoding under one shared reservation until the actual writer returns.
func WriteVerifyAuditAnchorsFile(ctx context.Context, cfg config.Config, path string, output io.Writer) (resultErr error) {
	ctx, release, err := anchorCommandContext(ctx, cfg)
	if err != nil {
		return err
	}
	defer func() {
		if !errors.Is(resultErr, errAnchorRetirement) {
			resultErr = errors.Join(resultErr, release())
		}
		resultErr = errors.Join(resultErr, ctx.Err())
	}()
	anchors, err := loadAuditAnchorsFileContext(ctx, path)
	if err != nil {
		return err
	}
	verdicts, err := VerifyAuditAnchors(ctx, cfg, anchors)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return json.NewEncoder(output).Encode(verdicts)
}
