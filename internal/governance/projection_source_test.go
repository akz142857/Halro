package governance

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/ownedfile"
	bbolt "go.etcd.io/bbolt"
)

func TestGovernanceProjectionSourceMutationBeforePublicationRejectsCandidate(t *testing.T) {
	for _, shape := range []string{"path-replaced", "path-deleted", "old-byte-tampered", "old-byte-tampered-restored-time", "complete-tail-deleted"} {
		t.Run(shape, func(t *testing.T) {
			_, log, _, _, _ := testManager(t)
			for i := 0; i < 3; i++ {
				if _, err := log.Append(context.Background(), projectionEvent(i)); err != nil {
					t.Fatal(err)
				}
			}
			original, err := os.ReadFile(log.path)
			if err != nil {
				t.Fatal(err)
			}
			output := filepath.Join(t.TempDir(), "view.db")
			changed := false
			p, err := buildGovernanceProjection(context.Background(), log.path, log.key, output, log.Summary(), func(phase string) {
				if phase != "source-reduced" {
					return
				}
				changed = true
				switch shape {
				case "path-replaced":
					if err := os.Rename(log.path, log.path+".old"); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(log.path, original, 0600); err != nil {
						t.Fatal(err)
					}
				case "path-deleted":
					if err := os.Remove(log.path); err != nil {
						t.Fatal(err)
					}
				case "old-byte-tampered", "old-byte-tampered-restored-time":
					info, err := os.Stat(log.path)
					if err != nil {
						t.Fatal(err)
					}
					bad := bytes.Clone(original)
					bad[frameHeaderSize+10] ^= 1
					if err := os.WriteFile(log.path, bad, 0600); err != nil {
						t.Fatal(err)
					}
					if shape == "old-byte-tampered-restored-time" {
						if err := os.Chtimes(log.path, info.ModTime(), info.ModTime()); err != nil {
							t.Fatal(err)
						}
					}
				case "complete-tail-deleted":
					// Use an actual earlier native frame boundary, not a synthetic length.
					var cut int64
					if _, err := log.Replay(func(r Record) error {
						if r.Sequence == 2 {
							cut = r.Offset
						}
						return nil
					}); err != nil {
						t.Fatal(err)
					}
					if err := os.Truncate(log.path, cut); err != nil {
						t.Fatal(err)
					}
				}
			}, nil)
			if p != nil {
				defer p.Discard()
			}
			if !changed || p != nil || !errors.Is(err, ErrCorrupt) {
				t.Fatal("changed source published", changed, p, err)
			}
			if _, err := os.Stat(output); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("failed candidate remained", err)
			}
			if shape == "path-replaced" {
				got, err := os.ReadFile(log.path)
				if err != nil || !bytes.Equal(got, original) {
					t.Fatal("foreign source changed", err)
				}
			}
		})
	}
}

func TestGovernanceProjectionActualSourceCloseBeforePublication(t *testing.T) {
	for _, shape := range []string{"success", "close-error", "cancel-after-close"} {
		t.Run(shape, func(t *testing.T) {
			_, log, _, _, _ := testManager(t)
			if _, err := log.Append(context.Background(), projectionEvent(1)); err != nil {
				t.Fatal(err)
			}
			output := filepath.Join(t.TempDir(), "view.db")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			sentinel := errors.New("actual source close failed")
			closed, beforePublication, closeCalls := false, false, 0
			p, err := buildGovernanceProjection(ctx, log.path, log.key, output, log.Summary(), func(phase string) {
				if phase == "before-publication" {
					beforePublication = true
					if !closed {
						t.Error("candidate publication precedes actual source close")
					}
					if shape == "cancel-after-close" {
						cancel()
					}
				}
			}, func(f *os.File) error {
				closeCalls++
				err := f.Close()
				closed = err == nil
				if shape == "close-error" {
					return errors.Join(err, f.Close(), sentinel)
				}
				return err
			})
			if p != nil {
				defer p.Discard()
			}
			if !closed || closeCalls != 1 {
				t.Fatal("source closure not owned exactly once", closed, closeCalls)
			}
			switch shape {
			case "success":
				if err != nil || p == nil || !beforePublication {
					t.Fatal(p, err)
				}
			case "close-error":
				if p == nil || !errors.Is(err, sentinel) || !errors.Is(err, ownedfile.ErrRetirement) {
					t.Fatal("close failure published", p, err)
				}
				if _, e := p.ReadHead(context.Background()); !errors.Is(e, ErrUnavailable) {
					t.Fatal("cleanup-only candidate became readable", e)
				}
				if e := p.Discard(); !errors.Is(e, ownedfile.ErrRetirement) {
					t.Fatal("cleanup retry erased original source debt", e)
				}
			case "cancel-after-close":
				if p != nil || !errors.Is(err, context.Canceled) {
					t.Fatal("post-close cancellation published", p, err)
				}
			}
			if shape == "close-error" {
				if _, err := os.Stat(output); err != nil {
					t.Fatal("unknown actual source close candidate was removed", err)
				}
			} else if shape != "success" {
				if _, err := os.Stat(output); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("candidate not retired", err)
				}
			}
		})
	}
}

type sourceReadProbe struct {
	*bytes.Reader
	cancel context.CancelFunc
	reads  int
}

func (r *sourceReadProbe) Read(b []byte) (int, error) {
	r.reads++
	n, err := r.Reader.Read(b)
	r.cancel()
	return n, err
}

var _ io.ReadSeeker = (*sourceReadProbe)(nil)

// A canceled reader must not proceed from a header read to reading its payload,
// or let the EOF classification turn cancellation into an empty successful scan.
func TestGovernanceProjectionSourceCancellationBeforePayloadRead(t *testing.T) {
	_, log, _, _, _ := testManager(t)
	if _, err := log.Append(context.Background(), projectionEvent(1)); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(log.path)
	if err != nil {
		t.Fatal(err)
	}
	for _, input := range [][]byte{raw, nil} {
		ctx, cancel := context.WithCancel(context.Background())
		reader := &sourceReadProbe{Reader: bytes.NewReader(input), cancel: cancel}
		_, _, err = scanFrom(governanceContextReader{ctx: ctx, source: reader}, log.key, Summary{}, nil)
		cancel()
		if !errors.Is(err, context.Canceled) || reader.reads != 1 {
			t.Fatal("cancellation lost to payload/EOF", reader.reads, err)
		}
	}
}

func TestGovernanceProjectionSourceFailurePhysicallyClosesCandidate(t *testing.T) {
	_, log, _, _, _ := testManager(t)
	output := filepath.Join(t.TempDir(), "view.db")
	moved := output + ".owned"
	sentinel := errors.New("close rejected")
	p, err := buildGovernanceProjection(context.Background(), log.path, log.key, output, log.Summary(), nil, func(f *os.File) error {
		if err := os.Rename(output, moved); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(output, []byte("foreign"), 0600); err != nil {
			t.Fatal(err)
		}
		return errors.Join(f.Close(), sentinel)
	})
	if p != nil {
		defer p.Close()
	}
	if p == nil || !errors.Is(err, sentinel) || !errors.Is(err, ownedfile.ErrReplaced) || !errors.Is(err, ownedfile.ErrRetirement) {
		t.Fatal(p, err)
	}
	if _, err := p.ReadHead(context.Background()); !errors.Is(err, ErrUnavailable) {
		t.Fatal("cleanup-only failed constructor became readable", err)
	}
	if err := p.Discard(); !errors.Is(err, ownedfile.ErrRetirement) || !errors.Is(err, ownedfile.ErrReplaced) {
		t.Fatal("failed constructor lost original source/creation debt", err)
	}
	if raw, err := os.ReadFile(output); err != nil || string(raw) != "foreign" {
		t.Fatal("foreign candidate path was removed", err)
	}
	db, err := bbolt.Open(moved, 0600, &bbolt.Options{ReadOnly: true, Timeout: time.Second})
	if err != nil {
		t.Fatal("candidate FD remained open", err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
}
