package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"reflect"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/resourcebudget"
	"github.com/akz142857/Halro/internal/vault"
)

const haGroupRotationPublishAttempts = 4

// Interrupted, unreceipted children remain opaque. A later attempt obtains a
// NEW actual exclusive creator in a fixed bounded slot; it never unlinks or
// adopts the prior child just because its name is familiar.
func (r *haGroupFileRotation) createStage(ctx context.Context, n, kind int, path string, raw []byte) (*haGroupRotationFile, error) {
	m := r.members[n].owners.Components.Budget
	for a := range haGroupRotationPublishAttempts {
		candidate := path
		if a != 0 {
			candidate = fmt.Sprintf("%s.%d", path, a+1)
		}
		info, err := os.Lstat(candidate)
		if errors.Is(err, os.ErrNotExist) {
			return r.createFile(ctx, n, kind+1, candidate, raw)
		}
		if err != nil {
			return nil, err
		}
		if info.Size() > haGroupRotationIntentLimit || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
			return nil, ownedfile.ErrReplaced
		}
		if r.opaque[n][kind][a] == (resourcebudget.Permit{}) {
			r.opaque[n][kind][a], err = m.TryAcquireRecovery(resourcebudget.Cost{Disk: haGroupRotationIntentLimit})
			if err != nil {
				return nil, err
			}
		}
	}
	return nil, authenticatedstate.ErrLimit
}

func sameHAGroupBootstrapCipher(raw []byte, key []byte, want haMemoryStateBootstrap) bool {
	v, err := vault.New(key)
	if err != nil {
		return false
	}
	defer v.Close()
	p, err := v.DecryptCredential(haMemoryBootstrapCredential, "system", haMemoryBootstrapDomain, raw)
	defer clear(p)
	if err != nil {
		return false
	}
	b, err := decodeHAGroupBootstrapPlain(p)
	if err != nil {
		return false
	}
	defer b.clearKeys()
	return reflect.DeepEqual(*b, want)
}

// Target destinations are re-read from actual FD/name/parent before comparison.
// The authorization comes from the independently read group intent plus actual
// full Member/Ordering/native/root re-open, never from a target header.
func (r *haGroupFileRotation) publishMemberArtifact(ctx context.Context, n, kind int, path, destination string, raw []byte) error {
	i, e := r.intent, r.intent.Members[n]
	cfg := r.members[n].cfg
	beforeRaw, before, err := readHAGroupRotationBytes(destination)
	defer clear(beforeRaw)
	if errors.Is(err, os.ErrNotExist) && (kind == 1 || kind == 3 && len(e.SourceHistory) == 0) {
		r.files[n][kind+1], err = r.createFile(ctx, n, kind+1, destination, raw)
		return err
	}
	if err != nil {
		return err
	}
	newMatch, oldMatch := false, false
	switch kind {
	case 0:
		newMatch = sameHAGroupBootstrapCipher(beforeRaw, i.NewKey, e.Bootstrap)
		oldMatch = sameHAGroupBootstrapCipher(beforeRaw, i.OldKey, e.Bootstrap)
	case 1:
		key, inc, _, newErr := readHAGroupClusterKeyCandidate(cfg, i.NewKey)
		newMatch = newErr == nil && key == i.ClusterKey && inc == sha256.Sum256([]byte(e.Source.Member.Incarnation))
		clear(key[:])
		if !newMatch {
			key, inc, _, oldErr := readHAGroupClusterKeyCandidate(cfg, i.OldKey)
			oldMatch = oldErr == nil && key == i.ClusterKey && inc == sha256.Sum256([]byte(e.Source.Member.Incarnation))
			clear(key[:])
		}
	case 2:
		if i.Mode == "key_slots" {
			newMatch = sameHAGroupTargetUnlock(cfg, i, n, beforeRaw)
			oldMatch = bytes.Equal(beforeRaw, e.SourceUnlock)
		} else {
			newMatch = bytes.Equal(beforeRaw, i.NewKey)
			oldMatch = bytes.Equal(beforeRaw, i.OldKey)
		}
	case 3:
		entries, err := decodeHAGroupHistory(beforeRaw, i.NewKey, cfg)
		newMatch = err == nil && entries == e.History
		clearHAGroupHistoricalKeys(&entries)
		oldMatch = len(e.SourceHistory) != 0 && bytes.Equal(beforeRaw, e.SourceHistory)
	default:
		return authenticatedstate.ErrSealIneligible
	}
	if newMatch {
		return nil
	}
	if !oldMatch {
		return authenticatedstate.ErrSealIneligible
	}
	r.files[n][kind+1], err = r.createStage(ctx, n, kind, path, raw)
	if err != nil {
		return err
	}
	return r.files[n][kind+1].publishCaptured(ctx, destination, before)
}

func decodeHAGroupBootstrapPlain(raw []byte) (*haMemoryStateBootstrap, error) {
	if len(raw) > memoryStateBootstrapLimit {
		return nil, authenticatedstate.ErrLimit
	}
	b := &haMemoryStateBootstrap{}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(b); err != nil {
		b.clearKeys()
		return nil, err
	}
	if err := d.Decode(new(any)); err != io.EOF {
		b.clearKeys()
		return nil, authenticatedstate.ErrCorrupt
	}
	return b, nil
}
