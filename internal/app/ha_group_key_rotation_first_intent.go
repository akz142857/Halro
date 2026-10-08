package app

import (
	"context"
	"crypto/sha256"
	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/statecut"
	"os"
	"reflect"
)

// A missing peer capsule supplies no private resume grant. It is opened through
// its ordinary old-key factory first, and all independently selected sources,
// seven roots and Member must still exactly equal the captured pre-write cut.
// Only then can a NEW exclusive creator publish the identical original bytes.
func (r *haGroupFileRotation) completeMissingIntents(ctx context.Context, p *haGroupResumeKeys) error {
	if p == nil {
		return authenticatedstate.ErrSealIneligible
	}
	for n := 0; n < int(r.intent.Count); n++ {
		if !p.missing[n] {
			continue
		}
		f := r.members[n]
		source, err := statecut.CaptureHAKeyRotationSource(ctx, f.reopened.Owners)
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(source, r.intent.Members[n].Source) || !reflect.DeepEqual(f.bootstrap, r.intent.Members[n].Bootstrap) {
			return authenticatedstate.ErrSealIneligible
		}
		// Re-read the original already-captured capsule; do not re-encode or infer
		// creator ownership from a familiar old path or a candidate checksum.
		raw, info, err := r.readActualIntent(haGroupRotationIntentPath(r.intent.Members[0].DataDir, r.slot))
		if err != nil {
			return err
		}
		if sha256.Sum256(raw) != r.intentDigest || !sameHAGroupReadFile(info, r.intentNames[0]) {
			clear(raw)
			return authenticatedstate.ErrSealIneligible
		}
		path := haGroupRotationIntentPath(r.intent.Members[n].DataDir, r.slot)
		file, err := r.createFile(ctx, n, 0, path, raw)
		clear(raw)
		r.files[n][0] = file
		if err != nil {
			return err
		}
		info, err = os.Lstat(path)
		if err != nil {
			return err
		}
		p.names[n] = info
		r.intentNames[n] = info
		p.missing[n] = false
	}
	return ctx.Err()
}
