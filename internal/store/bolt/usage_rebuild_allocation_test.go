package bolt

import (
	"bytes"
	"path/filepath"
	"runtime"
	"testing"
)

// Measure a real committed replacement/deletion of an 8 MiB old derivative.
// Seed allocation and fixture IO precede measurement. This guards the previous
// payload clone; it is not an RSS/cgroup or bbolt freelist capacity acceptance.
func TestNodeDerivedOverwriteDoesNotCloneHistoricalValue(t *testing.T) {
	for _, operation := range []string{"put", "delete", "cursor-delete"} {
		t.Run(operation, func(t *testing.T) {
			s, err := openUsageMaintenanceForTest(t, filepath.Join(t.TempDir(), "metadata.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			old := bytes.Repeat([]byte{'x'}, 8<<20)
			if err := s.update(func(tx *Tx) error { return tx.Bucket(bucketMeta).Put(keyUsageCheckpoint, old) }); err != nil {
				t.Fatal(err)
			}
			old = nil
			runtime.GC()
			var before, after runtime.MemStats
			runtime.ReadMemStats(&before)
			err = s.update(func(tx *Tx) error {
				bucket := tx.Bucket(bucketMeta)
				switch operation {
				case "put":
					return bucket.Put(keyUsageCheckpoint, []byte(`{}`))
				case "delete":
					return bucket.Delete(keyUsageCheckpoint)
				default:
					c := bucket.Cursor()
					k, _ := c.Seek(keyUsageCheckpoint)
					if !bytes.Equal(k, keyUsageCheckpoint) {
						t.Fatal("fixture checkpoint absent")
					}
					return c.Delete()
				}
			})
			runtime.ReadMemStats(&after)
			if err != nil {
				t.Fatal(err)
			}
			if bytes := after.TotalAlloc - before.TotalAlloc; bytes >= 4<<20 {
				t.Fatalf("committed %s allocated %d bytes for old 8 MiB derivative", operation, bytes)
			} else {
				t.Logf("committed %s allocated %d bytes for old 8 MiB derivative", operation, bytes)
			}
		})
	}
}
