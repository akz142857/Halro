package metadatajournal

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestMetadataExactFormatProbeAuthenticatesWithoutRepairOrDowngrade(t *testing.T) {
	for _, version := range []uint8{1, 2} {
		t.Run(string(rune('0'+version)), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "metadata.journal")
			var l *Log
			var err error
			if version == 1 {
				l, err = StartEpoch(path, testKey(), EpochHeader{Epoch: 9, Reason: "probe"})
			} else {
				b := testBinding()
				l, err = StartBoundEpoch(context.Background(), path, testKey(), EpochHeader{Epoch: 9, Reason: "probe", Baseline: &b, FileRevision: 1}, 64<<20)
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err := l.Append([]Op{put("projects", "p", "v")}); err != nil {
				t.Fatal(err)
			}
			want := l.Head()
			if err := l.Close(); err != nil {
				t.Fatal(err)
			}
			got, err := ProbeExactContext(context.Background(), path, testKey(), 64<<20)
			if err != nil || got != want {
				t.Fatal(got, want, err)
			}
			if version == 2 {
				if old, err := Open(path, testKey()); !errors.Is(err, ErrIncompatibleFormat) || old != nil {
					t.Fatal(old, err)
				}
			}
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := ProbeExactContext(context.Background(), path, bytes.Repeat([]byte{33}, 32), 64<<20); !errors.Is(err, ErrCorrupt) {
				t.Fatal(err)
			}
			for _, bad := range [][]byte{append(bytes.Clone(before), 'H', 'M'), append(bytes.Clone(before[:4]), append([]byte{99}, before[5:]...)...), before[:len(before)-1]} {
				if err := os.WriteFile(path, bad, 0600); err != nil {
					t.Fatal(err)
				}
				if _, err := ProbeExactContext(context.Background(), path, testKey(), 64<<20); !errors.Is(err, ErrCorrupt) {
					t.Fatal(err)
				}
				after, err := os.ReadFile(path)
				if err != nil || !bytes.Equal(after, bad) {
					t.Fatal("probe repaired source", err)
				}
			}
		})
	}
}
