package metadatajournal

import (
	"os"
	"testing"
)

func TestReplicaOpenRequiresOrderingRepairForPartialTail(t *testing.T) {
	path, writer := newJournal(t)
	if _, err := writer.Append([]Op{put("projects", "one", "one")}); err != nil {
		t.Fatal(err)
	}
	intact := writer.Head()
	if _, err := writer.Append([]Op{put("projects", "two", "two")}); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(path, (intact.Offset+info.Size())/2); err != nil {
		t.Fatal(err)
	}
	key := testKey()
	if log, err := OpenReplica(path, key); log != nil || err == nil {
		t.Fatalf("partial Replica metadata journal=%v err=%v", log, err)
	}
	partial, err := os.Stat(path)
	if err != nil || partial.Size() <= intact.Offset {
		t.Fatalf("Replica open repaired metadata before ordering: size=%v err=%v", partial, err)
	}
	if err := RepairReplicaTail(path, key, intact.Epoch, intact.Sequence); err != nil {
		t.Fatal(err)
	}
	log, err := OpenReplica(path, key)
	if err != nil {
		t.Fatal(err)
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
}
