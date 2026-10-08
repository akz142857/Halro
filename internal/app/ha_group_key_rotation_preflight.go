package app

import (
	"context"
	"errors"
	"fmt"
	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/resourcebudget"
	"os"
)

// Charge every retained child in the fixed candidate namespace and prepay all
// six possible creator ceilings before any target wrap or native mutation.
// This inventory supplies no old-file creator or unlink permission.
func (r *haGroupFileRotation) preflightPublication(ctx context.Context) error {
	if r == nil || r.intent == nil || r.slot < 0 {
		return authenticatedstate.ErrSealIneligible
	}
	for n := 0; n < int(r.intent.Count); n++ {
		e := r.intent.Members[n]
		m := r.members[n].owners.Components.Budget
		paths := [4]string{haGroupRotationStagePath(e.DataDir, r.slot, "bootstrap"), haGroupRotationStagePath(e.DataDir, r.slot, "cluster"), haGroupRotationKeyStage(e, r.slot), haGroupRotationStagePath(e.DataDir, r.slot, "history")}
		for k, path := range paths {
			free := false
			for a := range haGroupRotationPublishAttempts {
				candidate := path
				if a != 0 {
					candidate = fmt.Sprintf("%s.%d", path, a+1)
				}
				info, err := os.Lstat(candidate)
				if errors.Is(err, os.ErrNotExist) {
					free = true
					continue
				}
				if err != nil {
					return err
				}
				if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() > haGroupRotationIntentLimit {
					return ownedfile.ErrReplaced
				}
				if r.opaque[n][k][a] == (resourcebudget.Permit{}) {
					r.opaque[n][k][a], err = m.TryAcquireRecovery(resourcebudget.Cost{Disk: haGroupRotationIntentLimit})
					if err != nil {
						return err
					}
				}
			}
			if !free {
				return authenticatedstate.ErrLimit
			}
		}
		for k := range r.prepaid[n] {
			if r.prepaid[n][k] != (resourcebudget.Permit{}) {
				continue
			}
			p, err := m.TryAcquireRecovery(resourcebudget.Cost{Disk: haGroupRotationIntentLimit})
			if err != nil {
				return err
			}
			r.prepaid[n][k] = p
		}
	}
	return ctx.Err()
}
func (r *haGroupFileRotation) createFile(ctx context.Context, n, k int, path string, raw []byte) (*haGroupRotationFile, error) {
	fee := r.prepaid[n][k]
	r.prepaid[n][k] = resourcebudget.Permit{}
	return createHAGroupRotationFileWithDisk(ctx, path, raw, r.members[n].owners.Components.Budget, ownedfile.CloseFile, fee)
}

func (r *haGroupFileRotation) inventoryRetainedIntent(n int, m *resourcebudget.Manager) error {
	if r.files[n][0] != nil || r.retainedIntent[n] != (resourcebudget.Permit{}) {
		return nil
	}
	var err error
	r.retainedIntent[n], err = m.TryAcquireRecovery(resourcebudget.Cost{Disk: haGroupRotationIntentLimit})
	return err
}
