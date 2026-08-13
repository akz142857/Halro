package logging

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRotationKeepsExactlyTheConfiguredGenerations(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "halro.log")
	sink, err := OpenSink(Options{Path: path, MaxSizeBytes: 32, MaxFiles: 3})
	if err != nil {
		t.Fatal(err)
	}
	defer sink.Close()
	// Each record is 20 bytes, so every second record crosses the 32-byte limit
	// and rotates. Five records leave the live file plus two generations.
	for _, record := range []string{"aaaa", "bbbb", "cccc", "dddd", "eeee"} {
		if _, err := sink.Write([]byte(strings.Repeat(record, 4) + "\n")); err != nil {
			t.Fatal(err)
		}
	}
	for generation, want := range map[int]string{0: "eeee", 1: "dddd", 2: "cccc"} {
		content, err := os.ReadFile(generationPath(path, generation))
		if err != nil {
			t.Fatalf("generation %d: %v", generation, err)
		}
		if !strings.Contains(string(content), want) {
			t.Fatalf("generation %d holds %q, want %q", generation, content, want)
		}
	}
	if _, err := os.Stat(generationPath(path, 3)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a generation past max_files survived: %v", err)
	}
}

// The limit bounds a disk, so the oldest content is what leaves. A ladder that
// renamed upward without removing the last generation first would overwrite the
// newest survivor with its predecessor — the file would still be named .1, and
// hold the wrong request.
func TestRotationDropsTheOldestGenerationFirst(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "halro.log")
	sink, err := OpenSink(Options{Path: path, MaxSizeBytes: 16, MaxFiles: 2})
	if err != nil {
		t.Fatal(err)
	}
	defer sink.Close()
	for _, record := range []string{"first", "second", "third"} {
		if _, err := sink.Write([]byte(record + strings.Repeat("=", 10) + "\n")); err != nil {
			t.Fatal(err)
		}
	}
	live, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	previous, err := os.ReadFile(generationPath(path, 1))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(live), "third") || !strings.Contains(string(previous), "second") {
		t.Fatalf("live=%q previous=%q", live, previous)
	}
	if strings.Contains(string(live), "first") || strings.Contains(string(previous), "first") {
		t.Fatalf("the oldest generation was kept instead of dropped: live=%q previous=%q", live, previous)
	}
}

// max_files: 1 is a legitimate answer to "bound this disk and keep nothing" —
// and the ladder has no slot to move the current file into, so it has to start
// the file over rather than leave it growing.
func TestSingleGenerationKeepsNoHistory(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "halro.log")
	sink, err := OpenSink(Options{Path: path, MaxSizeBytes: 16, MaxFiles: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer sink.Close()
	for _, record := range []string{"first", "second"} {
		if _, err := sink.Write([]byte(record + strings.Repeat("=", 10) + "\n")); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Stat(generationPath(path, 1)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a generation was kept with max_files 1: %v", err)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(content), "second") || strings.Contains(string(content), "first") {
		t.Fatalf("live file=%q", content)
	}
}

// A record larger than the whole limit is written whole. Rotating an empty file
// would spend a generation per record and leave the history holding one line
// each, which is the opposite of what a size bound is for.
func TestARecordLargerThanTheLimitIsNotSplitOrLooped(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "halro.log")
	sink, err := OpenSink(Options{Path: path, MaxSizeBytes: 8, MaxFiles: 3})
	if err != nil {
		t.Fatal(err)
	}
	defer sink.Close()
	record := []byte(strings.Repeat("x", 64) + "\n")
	written, err := sink.Write(record)
	if err != nil || written != len(record) {
		t.Fatalf("written=%d err=%v", written, err)
	}
	if _, err := os.Stat(generationPath(path, 1)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("an empty file was rotated: %v", err)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(content) != len(record) {
		t.Fatalf("record was not written whole: %d bytes", len(content))
	}
}

// Reopening appends. A restart that truncated its own log would delete the
// records explaining why it restarted.
func TestReopeningAppendsAndKeepsCountingTheExistingSize(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "halro.log")
	first, err := OpenSink(Options{Path: path, MaxSizeBytes: 64, MaxFiles: 2})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := first.Write([]byte("before restart\n")); err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	second, err := OpenSink(Options{Path: path, MaxSizeBytes: 64, MaxFiles: 2})
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	if _, err := second.Write([]byte("after restart\n")); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(content), "before restart") || !strings.Contains(string(content), "after restart") {
		t.Fatalf("content=%q", content)
	}
}

// A log a passer-by can read is a log that undoes the reason records are
// redacted before they arrive.
func TestTheLogFileAndItsDirectoryArePrivate(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "logs", "halro.log")
	sink, err := OpenSink(Options{Path: path, MaxSizeBytes: 64, MaxFiles: 2})
	if err != nil {
		t.Fatal(err)
	}
	defer sink.Close()
	fileInfo, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fileInfo.Mode().Perm() != FilePerm {
		t.Fatalf("log file mode is %v", fileInfo.Mode().Perm())
	}
	dirInfo, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if dirInfo.Mode().Perm() != DirPerm {
		t.Fatalf("log directory mode is %v", dirInfo.Mode().Perm())
	}
}

// A disk that fills must cost the operator their log file, not their log. The
// notice is written once; the records keep coming.
func TestAFailedFileWriteFallsBackWithOneNotice(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "halro.log")
	fallback := &bytes.Buffer{}
	sink, err := OpenSink(Options{Path: path, MaxSizeBytes: 64, MaxFiles: 2, Fallback: fallback})
	if err != nil {
		t.Fatal(err)
	}
	// Closing the descriptor underneath the sink is the cheapest stand-in for a
	// write that cannot land, and it exercises the same path.
	sink.file.Close()
	for _, record := range []string{"first\n", "second\n"} {
		if _, err := sink.Write([]byte(record)); err == nil {
			t.Fatal("a failed file write reported success")
		}
	}
	written := fallback.String()
	if !strings.Contains(written, "first") || !strings.Contains(written, "second") {
		t.Fatalf("records did not reach the fallback: %q", written)
	}
	if strings.Count(written, "is unavailable") != 1 {
		t.Fatalf("the unavailable notice repeated per record: %q", written)
	}
}

func TestOpenRefusesAnUnusableConfiguration(t *testing.T) {
	dir := t.TempDir()
	for _, test := range []struct {
		name    string
		options Options
	}{
		{"no path", Options{MaxSizeBytes: 1, MaxFiles: 1}},
		{"no size limit", Options{Path: filepath.Join(dir, "a.log"), MaxFiles: 1}},
		{"no file kept", Options{Path: filepath.Join(dir, "b.log"), MaxSizeBytes: 1}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if sink, err := OpenSink(test.options); err == nil {
				sink.Close()
				t.Fatal("an unusable sink was opened")
			}
		})
	}
}
