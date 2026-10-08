package nativephysical

import (
	"bytes"
	"context"
	"crypto/hmac"
	"encoding/json"
	"errors"
	"io"

	"github.com/akz142857/Halro/internal/resourcebudget"
)

// PreparedObservation is evidence about this original allocation only. The
// native parent supplies the authenticated current head while holding its lane.
// Neither the saved head nor these bytes select a root or permit an append.
type PreparedObservation struct {
	Identity                    [32]byte
	Controls                    [2]resourcebudget.PhysicalIdentity
	Native                      Reference
	Baseline                    Position
	AvailableBytes, AppendBytes int64
}

func (w *Window) ObservePrepared(ctx context.Context, head Position) (out PreparedObservation, resultErr error) {
	if w == nil {
		return out, ErrOwnership
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := errors.Join(ctx.Err(), w.verifyLocked(head)); err != nil {
		return out, err
	}
	if w.receipt.Head.Sequence > head.Sequence || w.receipt.Head.EOF > head.EOF || w.reservedBytes < 0 || w.reservedBytes > w.receipt.End-head.EOF {
		return out, ErrOwnership
	}
	for _, fee := range w.fees {
		if _, err := fee.Cost(); err != nil {
			return out, err
		}
	}
	for i, f := range w.controls {
		id, err := resourcebudget.CapturePhysicalIdentity(f)
		if err != nil || id != w.receipt.Banks[i] {
			return out, errors.Join(ErrOwnership, err)
		}
		var raw [4096]byte
		if n, err := f.ReadAt(raw[:], 0); err != nil || n != len(raw) {
			return out, errors.Join(io.ErrUnexpectedEOF, err)
		}
		var bank receipt
		if err := json.Unmarshal(bytes.TrimRight(raw[:], "\x00"), &bank); err != nil {
			return out, err
		}
		mac, err := mac(bank, w.key[:])
		if err != nil || !hmac.Equal(mac, bank.MAC) || bank.Version != 1 || bank.Identity != w.receipt.Identity || bank.Native != w.receipt.Native || bank.Parent != w.receipt.Parent || bank.Banks != w.receipt.Banks || bank.Serial > w.receipt.Serial || bank.Serial == w.receipt.Serial && !sameReceipt(bank, w.receipt) || i == int(w.receipt.Serial%2) && !sameReceipt(bank, w.receipt) {
			return out, errors.Join(ErrOwnership, err)
		}
		if err := resourcebudget.CheckPhysicalRange(f, 0, 4096); err != nil {
			return out, err
		}
	}
	if w.receipt.End > head.EOF {
		if err := resourcebudget.CheckPhysicalRange(w.file, head.EOF, w.receipt.End); err != nil {
			return out, err
		}
	}
	if err := w.opts.Authenticate(ctx, w.receipt.Head); err != nil {
		return out, err
	}
	if err := errors.Join(ctx.Err(), w.verifyLocked(head)); err != nil {
		return out, err
	}
	out.Identity, out.Controls = w.receipt.Identity, w.receipt.Banks
	out.Native = Reference{Native: w.receipt.Native, Parent: w.receipt.Parent, Head: head, End: w.receipt.End}
	out.Baseline, out.AvailableBytes, out.AppendBytes = w.receipt.Head, w.receipt.End-head.EOF-w.reservedBytes, w.opts.AppendBytes
	return out, nil
}
