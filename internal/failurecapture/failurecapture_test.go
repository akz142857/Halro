package failurecapture

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeSealer binds a record to its request and project the way the vault does,
// without needing a master key in the test process. The binding is what the
// tests here actually exercise; the cipher itself is the vault package's.
type fakeSealer struct{ failEncrypt bool }

func (f fakeSealer) EncryptFailurePayload(requestID, projectID string, plaintext []byte) ([]byte, error) {
	if f.failEncrypt {
		return nil, os.ErrPermission
	}
	return append([]byte(requestID+"|"+projectID+"|"), plaintext...), nil
}

func (f fakeSealer) DecryptFailurePayload(requestID, projectID string, envelope []byte) ([]byte, error) {
	prefix := requestID + "|" + projectID + "|"
	if !strings.HasPrefix(string(envelope), prefix) {
		return nil, os.ErrInvalid
	}
	return envelope[len(prefix):], nil
}

func newStore(t *testing.T, mutate func(*Options)) (*Store, string) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "failures")
	options := Options{
		Root: root, MaxBytes: 1024, MaxRecordsPerDay: 10,
		Retain: 24 * time.Hour,
		Now:    func() time.Time { return time.Date(2026, 9, 2, 10, 0, 0, 0, time.UTC) },
	}
	if mutate != nil {
		mutate(&options)
	}
	store, err := Open(fakeSealer{}, options)
	if err != nil {
		t.Fatal(err)
	}
	return store, options.Root
}

func record(requestID string) Record {
	return Record{
		RequestID: requestID, ProjectID: "project_1", Outcome: "provider_error",
		Request:  json.RawMessage(`{"model":"chat","messages":[{"role":"user","content":"hello"}]}`),
		Response: json.RawMessage(`{"provider_status":400,"body":"invalid image url"}`),
	}
}

func TestACapturedFailureRoundTrips(t *testing.T) {
	store, _ := newStore(t, nil)
	written, err := store.Put(record("req_1"))
	if err != nil || !written {
		t.Fatalf("written=%v err=%v", written, err)
	}
	got, found, err := store.Get("req_1", "project_1")
	if err != nil || !found {
		t.Fatalf("found=%v err=%v", found, err)
	}
	if string(got.Request) != `{"model":"chat","messages":[{"role":"user","content":"hello"}]}` ||
		got.Outcome != "provider_error" || got.CapturedAt.IsZero() {
		t.Fatalf("record = %#v", got)
	}
}

// The envelope is bound to the request and the project. Reading a capture under
// another project must fail rather than succeed with somebody else's traffic —
// that binding is what stops a record being renamed onto a different request or
// lifted into another install's directory.
func TestACaptureCannotBeOpenedUnderAnotherProject(t *testing.T) {
	store, _ := newStore(t, nil)
	if _, err := store.Put(record("req_1")); err != nil {
		t.Fatal(err)
	}
	if _, found, err := store.Get("req_1", "project_2"); err == nil && found {
		t.Fatal("a capture opened under the wrong project")
	}
	if _, found, _ := store.Get("req_missing", "project_1"); found {
		t.Fatal("a capture that was never written was found")
	}
}

// The request ID becomes a file name. Halro's own IDs cannot contain a
// separator, but one supplied by a caller reaches here, so this is the line
// that decides where a file lands.
func TestACaptureRefusesARequestIDThatIsNotAFileName(t *testing.T) {
	store, root := newStore(t, nil)
	for _, requestID := range []string{"../../master.key", "a/b", `a\b`, "req.1"} {
		hostile := record(requestID)
		if written, err := store.Put(hostile); err == nil || written {
			t.Fatalf("%q was accepted as a file name", requestID)
		}
	}
	// And nothing landed outside the store.
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("the store is not empty: %v", entries)
	}
}

// A ceiling that could be exceeded is not a ceiling. The truncated side is
// flagged, so a reader does not diagnose a malformed body that is only an
// incomplete one, and it stays valid JSON — a cut-off document does not parse,
// and one that does not parse cannot be told apart from a broken upstream.
func TestAnOversizedCaptureIsTruncatedAndFlagged(t *testing.T) {
	store, _ := newStore(t, func(options *Options) { options.MaxBytes = 64 })
	large := record("req_1")
	large.Request = json.RawMessage(`{"prompt":"` + strings.Repeat("x", 4096) + `"}`)
	if _, err := store.Put(large); err != nil {
		t.Fatal(err)
	}
	got, found, err := store.Get("req_1", "project_1")
	if err != nil || !found {
		t.Fatalf("found=%v err=%v", found, err)
	}
	if !got.RequestTruncated {
		t.Fatal("an oversized request was stored without saying it was cut")
	}
	if len(got.Request) > 128 {
		t.Fatalf("the ceiling did not apply: %d bytes", len(got.Request))
	}
	var decoded any
	if err := json.Unmarshal(got.Request, &decoded); err != nil {
		t.Fatalf("a truncated capture is no longer decodable: %v", err)
	}
	// The response side is bounded separately: a large answer must not cost the
	// request that explains it.
	if got.ResponseTruncated {
		t.Fatal("a small response was truncated along with a large request")
	}
}

// An upstream failing everything must not be able to grow this store without
// bound — it shares a disk with the ledger the accounting depends on.
func TestCaptureStopsAtTheDailyCeilingAndSaysSoOnce(t *testing.T) {
	store, _ := newStore(t, func(options *Options) { options.MaxRecordsPerDay = 3 })
	for index := range 3 {
		written, err := store.Put(record("req_" + string(rune('a'+index))))
		if err != nil || !written {
			t.Fatalf("record %d: written=%v err=%v", index, written, err)
		}
	}
	if store.Saturated() {
		t.Fatal("the store reported saturation while still under its ceiling")
	}
	written, err := store.Put(record("req_over"))
	if err != nil {
		t.Fatal(err)
	}
	if written {
		t.Fatal("a capture past the daily ceiling was written")
	}
	if !store.Saturated() {
		t.Fatal("the ceiling was reached and nothing said so")
	}
	// Once per day, so a runaway upstream produces one line rather than one per
	// dropped capture.
	if store.Saturated() {
		t.Fatal("saturation was reported twice for the same day")
	}
	if _, found, _ := store.Get("req_over", "project_1"); found {
		t.Fatal("a dropped capture was stored anyway")
	}
}

// Retention is the answer an operator gives their own compliance people, so the
// configured value has to be the number that is true. Expiry is per record: day
// granularity made `retain` a floor rather than a ceiling, keeping a record
// configured for an hour the better part of a day.
func TestRetentionExpiresEachRecordOnTheConfiguredWindow(t *testing.T) {
	now := time.Date(2026, 9, 5, 10, 0, 0, 0, time.UTC)
	clock := now
	store, root := newStore(t, func(options *Options) {
		options.Retain = time.Hour
		options.Now = func() time.Time { return clock }
	})
	// All three land in the same day directory, and only the window decides.
	// Under day granularity every one of these survived.
	for _, written := range []struct {
		at        time.Time
		requestID string
	}{
		{now.Add(-90 * time.Minute), "req_expired"},
		{now.Add(-30 * time.Minute), "req_recent"},
		{now, "req_now"},
	} {
		clock = written.at
		if _, err := store.Put(record(written.requestID)); err != nil {
			t.Fatal(err)
		}
	}
	clock = now
	if err := store.Purge(); err != nil {
		t.Fatal(err)
	}
	if _, found, _ := store.Get("req_expired", "project_1"); found {
		t.Fatal("a capture past the retention window survived the sweep")
	}
	for _, requestID := range []string{"req_recent", "req_now"} {
		if _, found, _ := store.Get(requestID, "project_1"); !found {
			t.Fatalf("%s was swept while still inside the window", requestID)
		}
	}
	// A day whose every record has expired leaves no directory behind.
	clock = now.Add(2 * time.Hour)
	if err := store.Purge(); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("an emptied day directory was left behind: %v", entries)
	}
	clock = now
	// A directory the sweep does not recognise is left alone rather than
	// removed: this store deletes on a clock, and deleting what it cannot date
	// is how a retention pass becomes a data loss incident.
	foreign := filepath.Join(root, "not-a-day")
	if err := os.MkdirAll(foreign, DirPerm); err != nil {
		t.Fatal(err)
	}
	if err := store.Purge(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(foreign); err != nil {
		t.Fatalf("the sweep removed a directory it could not date: %v", err)
	}
}

// The content is more sensitive than anything else in the data directory, and
// "encrypted" is not a licence for the file to be world-readable.
func TestCapturesAreAsPrivateAsTheDataDirectory(t *testing.T) {
	store, root := newStore(t, nil)
	if _, err := store.Put(record("req_1")); err != nil {
		t.Fatal(err)
	}
	day := filepath.Join(root, "2026-09-02")
	directory, err := os.Stat(day)
	if err != nil {
		t.Fatal(err)
	}
	if directory.Mode().Perm() != DirPerm {
		t.Fatalf("directory mode = %v, want %v", directory.Mode().Perm(), DirPerm)
	}
	entries, err := os.ReadDir(day)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("day = %v", entries)
	}
	// The name carries the capture instant as well as the request, which is
	// what lets the sweep date a record without opening it.
	if name, _, ours := capturedRequestID(entries[0].Name()); !ours || name != "req_1" {
		t.Fatalf("file name = %q", entries[0].Name())
	}
	file, err := os.Stat(filepath.Join(day, entries[0].Name()))
	if err != nil {
		t.Fatal(err)
	}
	if file.Mode().Perm() != FilePerm {
		t.Fatalf("file mode = %v, want %v", file.Mode().Perm(), FilePerm)
	}
	// And the material really is sealed on disk, rather than sitting in the
	// clear behind a file mode.
	raw, err := os.ReadFile(filepath.Join(day, entries[0].Name()))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(raw), "req_1|project_1|") {
		t.Fatal("the record was written without going through the sealer")
	}
}

// Every bound has to be supplied. A store with no expiry is a store of customer
// prompts kept forever, which is the thing this package exists to prevent.
func TestOpenRefusesAStoreWithoutItsBounds(t *testing.T) {
	base := Options{Root: filepath.Join(t.TempDir(), "failures"), MaxBytes: 1024, MaxRecordsPerDay: 10, Retain: time.Hour}
	for name, mutate := range map[string]func(*Options){
		"no root":       func(o *Options) { o.Root = "" },
		"no byte limit": func(o *Options) { o.MaxBytes = 0 },
		"no day limit":  func(o *Options) { o.MaxRecordsPerDay = 0 },
		"no retention":  func(o *Options) { o.Retain = 0 },
	} {
		t.Run(name, func(t *testing.T) {
			options := base
			mutate(&options)
			if _, err := Open(fakeSealer{}, options); err == nil {
				t.Fatal("an unbounded store was opened")
			}
		})
	}
	if _, err := Open(nil, base); err == nil {
		t.Fatal("a store was opened with nothing to seal with")
	}
}

// The ceiling has to bound what is stored, not the prefix taken before
// escaping. Cutting raw bytes and marshalling afterwards let a
// control-character payload out at roughly six times the configured size —
// every byte becoming \u00XX — which is not a ceiling an operator can size a
// disk against.
func TestTheCeilingBoundsWhatIsStoredNotWhatWasCut(t *testing.T) {
	const ceiling = 256
	store, _ := newStore(t, func(options *Options) { options.MaxBytes = ceiling })
	hostile := record("req_1")
	// Every byte escapes to six.
	hostile.Request = json.RawMessage(`"` + strings.Repeat("\x01", 4096) + `"`)
	hostile.Response = json.RawMessage(`"` + strings.Repeat("\x02", 4096) + `"`)
	if _, err := store.Put(hostile); err != nil {
		t.Fatal(err)
	}
	got, found, err := store.Get("req_1", "project_1")
	if err != nil || !found {
		t.Fatalf("found=%v err=%v", found, err)
	}
	for name, side := range map[string]json.RawMessage{"request": got.Request, "response": got.Response} {
		if len(side) > ceiling {
			t.Fatalf("%s stored %d bytes against a ceiling of %d", name, len(side), ceiling)
		}
		var decoded any
		if err := json.Unmarshal(side, &decoded); err != nil {
			t.Fatalf("%s is no longer decodable: %v", name, err)
		}
	}
	if !got.RequestTruncated || !got.ResponseTruncated {
		t.Fatal("a cut side was stored without saying it was cut")
	}
}

// The write is atomic because the moment it is most likely to be interrupted is
// exactly when captures are densest: an upstream failing under load is also when
// a process is most likely to be killed. A truncated GCM envelope fails
// authentication on every read, so the admin endpoint reports "no captured
// payload" — the operator is told nothing was captured while the file sits there
// until its retention expires.
func TestACaptureIsPublishedWholeAndLeavesNoTemporaryFile(t *testing.T) {
	store, root := newStore(t, nil)
	if written, err := store.Put(record("req_atomic")); err != nil || !written {
		t.Fatalf("written=%v err=%v", written, err)
	}
	days, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, day := range days {
		if !day.IsDir() {
			continue
		}
		entries, err := os.ReadDir(filepath.Join(root, day.Name()))
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range entries {
			if strings.HasPrefix(entry.Name(), ".capture-") {
				t.Fatalf("a temporary file survived the write: %s", entry.Name())
			}
			if !strings.HasSuffix(entry.Name(), ".hfc") {
				t.Fatalf("unexpected entry in a capture day: %s", entry.Name())
			}
		}
	}
	if _, found, err := store.Get("req_atomic", "project_1"); err != nil || !found {
		t.Fatalf("the published capture could not be read back: found=%v err=%v", found, err)
	}
}
