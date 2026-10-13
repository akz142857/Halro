package resourcebudget

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func samplerFixture(t *testing.T, member, root, point string) (*Sampler, map[string]string) {
	t.Helper()
	s, err := NewSampler("/data")
	if err != nil {
		t.Fatal(err)
	}
	files := map[string]string{"/proc/self/cgroup": "0::" + member + "\n", "/proc/self/mountinfo": fmt.Sprintf("31 20 0:30 %s %s ro - cgroup2 cgroup rw\n", root, point)}
	mount, err := locateCgroup(member, []byte(files["/proc/self/mountinfo"]))
	if err != nil {
		t.Fatal(err)
	}
	for path := mount.leaf; ; path = filepath.Dir(path) {
		files[filepath.Join(path, "memory.current")] = "100\n"
		files[filepath.Join(path, "memory.max")] = "500\n"
		files[filepath.Join(path, "memory.high")] = "max\n"
		if path == mount.point {
			break
		}
	}
	s.host = nil // these cases exercise the cgroup path on every platform
	s.now = func() time.Time { return time.Unix(1000, 0) }
	s.runtime = func() (int64, error) { return 50, nil }
	s.disk = func(string) (diskSignal, error) { return diskSignal{500, 50}, nil }
	s.verifyCgroup = func(string) ([16]byte, error) { return [16]byte{1}, nil }
	s.read = func(path string, limit int) ([]byte, error) {
		text, ok := files[path]
		if !ok {
			return nil, os.ErrNotExist
		}
		if len(text) > limit {
			return nil, ErrInvalid
		}
		return []byte(text), nil
	}
	return s, files
}

func TestNewSamplerTracksDataAndSystemTemporaryDirectories(t *testing.T) {
	s, err := NewSampler("/data")
	if err != nil {
		t.Fatal(err)
	}
	scratch, err := filepath.Abs(os.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	want := []string{filepath.Clean("/data")}
	if filepath.Clean(scratch) != want[0] {
		want = append(want, filepath.Clean(scratch))
	}
	if len(s.diskPaths) != len(want) {
		t.Fatal("disk sample paths", s.diskPaths, "want", want)
	}
	for i := range want {
		if s.diskPaths[i] != want[i] {
			t.Fatal("disk sample paths", s.diskPaths, "want", want)
		}
	}
}

func TestSamplerCollectsVisibleHierarchyAndUnlimitedSeparately(t *testing.T) {
	for _, tc := range []struct {
		member, root, point string
		count               int
	}{{"/team/work", "/", "/cg", 3}, {"/team/work", "/team", "/cg", 2}, {"/team/work", "/", "/", 3}, {"/space name/work", "/", "/cg\\040name", 3}} {
		s, files := samplerFixture(t, tc.member, tc.root, tc.point)
		mount, err := locateCgroup(tc.member, []byte(files["/proc/self/mountinfo"]))
		if err != nil {
			t.Fatal(err)
		}
		files[filepath.Join(mount.leaf, "memory.current")] = "0\n"
		files[filepath.Join(mount.leaf, "memory.max")] = "0\n"
		sample := s.Sample(context.Background())
		if sample.Issues != 0 || sample.GroupCount != tc.count || sample.CgroupID == ([32]byte{}) || sample.Groups[0].Current != 0 || !sample.Groups[0].MaxLimited || sample.Groups[0].Max != 0 || sample.Groups[0].HighLimited {
			t.Fatal(tc, sample)
		}
	}
}

func TestSamplerUsesLowestDataAndScratchFilesystemHeadroom(t *testing.T) {
	s, _ := samplerFixture(t, "/work", "/", "/cg")
	s.diskPaths = []string{"/data", "/scratch"}
	s.disk = func(path string) (diskSignal, error) {
		switch path {
		case "/data":
			return diskSignal{available: 0, inodes: 0}, nil
		case "/scratch":
			return diskSignal{available: 500, inodes: 50}, nil
		default:
			return diskSignal{}, os.ErrNotExist
		}
	}
	sample := s.Sample(context.Background())
	if sample.Issues != 0 || sample.DiskAvailable != 0 || sample.InodesAvailable != 0 {
		t.Fatal("zero headroom on either owned filesystem must remain visible", sample)
	}

	s.disk = func(path string) (diskSignal, error) {
		if path == "/scratch" {
			return diskSignal{}, os.ErrNotExist
		}
		return diskSignal{available: 500, inodes: 50}, nil
	}
	sample = s.Sample(context.Background())
	if sample.Issues&DiskUnavailable == 0 || sample.DiskAvailable != 0 {
		t.Fatal("unknown scratch filesystem must fail closed", sample)
	}

	s.disk = func(path string) (diskSignal, error) {
		switch path {
		case "/data":
			return diskSignal{available: 500, inodes: 50}, nil
		case "/scratch":
			return diskSignal{available: 400, inodes: 40}, nil
		case "/backup-output":
			return diskSignal{available: 300, inodes: 30}, nil
		default:
			return diskSignal{}, os.ErrNotExist
		}
	}
	available, inodes, err := s.DiskHeadroom(context.Background(), "/backup-output")
	if err != nil || available != 300 || inodes != 30 {
		t.Fatal("operation output filesystem was not included", available, inodes, err)
	}
	if _, _, err := s.DiskHeadroom(context.Background(), "relative"); err == nil {
		t.Fatal("relative operation filesystem accepted")
	}
}

func TestSamplerCancellationBetweenFilesystemChecksStopsSampling(t *testing.T) {
	s, _ := samplerFixture(t, "/work", "/", "/cg")
	s.diskPaths = []string{"/data", "/scratch"}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	calls := 0
	s.disk = func(string) (diskSignal, error) {
		calls++
		cancel()
		return diskSignal{available: 500, inodes: 50}, nil
	}
	sample := s.Sample(ctx)
	if sample.Issues&SampleCancelled == 0 || calls != 1 {
		t.Fatal("cancelled sample continued to another filesystem", calls, sample)
	}
}

func TestSamplerRootAbsenceAllowedButMissingOtherSignalsFailClosed(t *testing.T) {
	s, files := samplerFixture(t, "/work", "/", "/cg")
	for _, name := range []string{"memory.current", "memory.max", "memory.high"} {
		delete(files, "/cg/"+name)
	}
	sample := s.Sample(context.Background())
	if sample.Issues != 0 || sample.GroupCount != 1 {
		t.Fatal(sample)
	}
	s, files = samplerFixture(t, "/team/work", "/", "/cg")
	delete(files, "/cg/team/memory.current")
	sample = s.Sample(context.Background())
	if sample.Issues&CgroupUnavailable == 0 || sample.GroupCount != 0 || sample.CgroupID != ([32]byte{}) {
		t.Fatal("missing intermediate must not certify healthy", sample)
	}
	s, files = samplerFixture(t, "/", "/", "/cg")
	for _, name := range []string{"memory.current", "memory.max", "memory.high"} {
		delete(files, "/cg/"+name)
	}
	if sample = s.Sample(context.Background()); sample.Issues&CgroupUnavailable == 0 {
		t.Fatal("no leaf signal", sample)
	}
}
func TestSamplerRejectsMalformedMembershipMountAndScalar(t *testing.T) {
	for _, text := range []string{"", "1:memory:/work\n", "0::/work\n0::/work\n", "0::/../work\n", "0::relative\n", "0::/work (deleted)\n", "0::/tab\tname\n"} {
		if _, err := membership([]byte(text)); err == nil {
			t.Fatal("membership accepted", text)
		}
	}
	for _, text := range []string{"-1", "+1", "9223372036854775808", "max", "", "1x"} {
		if _, _, err := scalar([]byte(text), false); err == nil {
			t.Fatal("scalar accepted", text)
		}
	}
	if _, limited, err := scalar([]byte("max\n"), true); err != nil || limited {
		t.Fatal(limited, err)
	}
	for _, text := range []string{`/cg\011x`, `/cg\012x`, `/cg\999`, `/cg\`, `/cg/../x`} {
		if _, err := mountPath(text); err == nil {
			t.Fatal("mount accepted", text)
		}
	}
	if p, err := mountPath(`/cg\040a\134b`); err != nil || p != "/cg a\\b" {
		t.Fatal(p, err)
	}
	for _, text := range []string{"31 20 0:30 / /cg ro - cgroup cgroup rw\n", "31 20 0:30 /team /cg ro - cgroup2 cgroup rw\n", "31 20 0:30 / /cg ro - cgroup2 cgroup rw\n32 20 0:30 / /other ro - cgroup2 cgroup rw\n"} {
		if _, err := locateCgroup("/work", []byte(text)); err == nil {
			t.Fatal("mount accepted", text)
		}
	}
	s, files := samplerFixture(t, "/work", "/", "/cg")
	files["/cg/work/memory.max"] = "9223372036854775808"
	if sample := s.Sample(context.Background()); sample.Issues&CgroupUnavailable == 0 {
		t.Fatal(sample)
	}
}
func TestSamplerDetectsMovementAndRemountDuringCollection(t *testing.T) {
	for _, path := range []string{"/proc/self/cgroup", "/proc/self/mountinfo"} {
		s, _ := samplerFixture(t, "/work", "/", "/cg")
		read := s.read
		count := 0
		s.read = func(p string, l int) ([]byte, error) {
			b, err := read(p, l)
			if p == path {
				count++
				if count == 2 {
					b = append(b, byte(' '))
				}
			}
			return b, err
		}
		sample := s.Sample(context.Background())
		if sample.Issues&CgroupUnavailable == 0 || sample.GroupCount != 0 {
			t.Fatal(path, sample)
		}
	}
	s, _ := samplerFixture(t, "/work", "/", "/cg")
	count := 0
	s.verifyCgroup = func(string) ([16]byte, error) {
		count++
		if count == 2 {
			return [16]byte{}, ErrInvalid
		}
		return [16]byte{1}, nil
	}
	if sample := s.Sample(context.Background()); sample.Issues&CgroupUnavailable == 0 {
		t.Fatal("filesystem changed", sample)
	}
}
func TestUnrelatedStableMountChangeDoesNotChangeCgroupSource(t *testing.T) {
	s, files := samplerFixture(t, "/work", "/", "/cg")
	a := s.Sample(context.Background())
	files["/proc/self/mountinfo"] += "32 20 0:31 / /unrelated ro - tmpfs tmpfs rw\n"
	b := s.Sample(context.Background())
	if a.Issues != 0 || b.Issues != 0 || a.CgroupID != b.CgroupID {
		t.Fatal(a, b)
	}
}
func TestSamplerCancellationMissingSignalsAndBoundedReads(t *testing.T) {
	s, _ := samplerFixture(t, "/work", "/", "/cg")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if sample := s.Sample(ctx); sample.Issues != SampleCancelled {
		t.Fatal(sample)
	}
	s.runtime = func() (int64, error) { return 0, ErrInvalid }
	s.disk = func(string) (diskSignal, error) { return diskSignal{}, ErrInvalid }
	s.verifyCgroup = func(string) ([16]byte, error) { return [16]byte{}, ErrInvalid }
	if sample := s.Sample(context.Background()); sample.Issues != (RuntimeUnavailable | DiskUnavailable | CgroupUnavailable) {
		t.Fatal(sample)
	}
	s, _ = samplerFixture(t, "/work", "/", "/cg")
	ctx, cancel = context.WithCancel(context.Background())
	s.runtime = func() (int64, error) { cancel(); return 50, nil }
	if sample := s.Sample(ctx); sample.Issues&SampleCancelled == 0 {
		t.Fatal(sample)
	}
	path := filepath.Join(t.TempDir(), "oversized")
	if err := os.WriteFile(path, []byte(strings.Repeat("x", scalarLimit+1)), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := boundedText(path, scalarLimit); err == nil {
		t.Fatal("oversized accepted")
	}
	if b, err := boundedText(path, scalarLimit+1); err != nil || len(b) != scalarLimit+1 {
		t.Fatal(len(b), err)
	}
	s, _ = samplerFixture(t, "/"+strings.Repeat("a/", MaxCgroupDepth)+"leaf", "/", "/cg")
	if sample := s.Sample(context.Background()); sample.Issues&CgroupUnavailable == 0 {
		t.Fatal("hierarchy grew beyond fixed array", sample)
	}
	if _, err := NewSampler("relative"); err == nil {
		t.Fatal("relative data dir")
	}
	if used, err := runtimeSignal(); err != nil || used <= 0 {
		t.Fatal(used, err)
	}
}
func TestSlowSamplingKeepsStartTimeSoDecisionExpires(t *testing.T) {
	s, _ := samplerFixture(t, "/work", "/", "/cg")
	start := s.now()
	now := start
	s.disk = func(string) (diskSignal, error) { now = now.Add(11 * time.Second); return diskSignal{500, 50}, nil }
	sample := s.Sample(context.Background())
	if sample.At != start || sample.Issues != 0 {
		t.Fatal(sample)
	}
	d := testController(t).Observe(sample, now)
	if d.State() != Unknown || d.Issues()&SampleStale == 0 {
		t.Fatal(d)
	}
}

func TestSamplerSelectedMountOrLeafReplacementResetsCooling(t *testing.T) {
	for _, kind := range []string{"mount-id", "device", "directory"} {
		t.Run(kind, func(t *testing.T) {
			s, files := samplerFixture(t, "/work", "/", "/cg")
			before := s.Sample(context.Background())
			switch kind {
			case "mount-id":
				files["/proc/self/mountinfo"] = strings.Replace(files["/proc/self/mountinfo"], "31 20", "32 20", 1)
			case "device":
				files["/proc/self/mountinfo"] = strings.Replace(files["/proc/self/mountinfo"], "0:30", "0:31", 1)
			case "directory":
				s.verifyCgroup = func(string) ([16]byte, error) { return [16]byte{2}, nil }
			}
			after := s.Sample(context.Background())
			if before.Issues != 0 || after.Issues != 0 || before.CgroupID == after.CgroupID {
				t.Fatal(before, after)
			}
			c := testController(t)
			at := before.At
			_ = c.Observe(before, at)
			before.At = at.Add(2 * time.Second)
			if d := c.Observe(before, before.At); d.State() != Healthy {
				t.Fatal(d)
			}
			after.At = at.Add(3 * time.Second)
			after.RuntimeUsed = 90
			d := c.Observe(after, after.At)
			if d.State() != Unknown || d.Issues()&SourceChanged == 0 {
				t.Fatal("replacement inherited healthy middle band", d)
			}
		})
	}
	s, _ := samplerFixture(t, "/work", "/", "/cg")
	calls := 0
	s.verifyCgroup = func(string) ([16]byte, error) { calls++; return [16]byte{byte(calls)}, nil }
	if sample := s.Sample(context.Background()); sample.Issues&CgroupUnavailable == 0 {
		t.Fatal("directory replacement during sample", sample)
	}
}

func TestSamplerHostGroupStandsInForAMissingCgroup(t *testing.T) {
	s, _ := samplerFixture(t, "/work", "/", "/cg")
	s.read = func(string, int) ([]byte, error) { return nil, os.ErrNotExist }
	identity := [32]byte{7}
	s.host = func() (hostMemory, error) { return hostMemory{available: 400, identity: identity}, nil }
	sample := s.Sample(context.Background())
	if sample.Issues != 0 || sample.GroupCount != 1 || sample.CgroupID != identity || sample.Groups[0] != (GroupMemory{Current: 50, Max: 450, MaxLimited: true}) {
		t.Fatal("host group", sample)
	}
	for name, host := range map[string]func() (hostMemory, error){
		"unreadable":       func() (hostMemory, error) { return hostMemory{}, os.ErrNotExist },
		"negative":         func() (hostMemory, error) { return hostMemory{available: -1, identity: identity}, nil },
		"overflow":         func() (hostMemory, error) { return hostMemory{available: 1<<63 - 1, identity: identity}, nil },
		"missing-identity": func() (hostMemory, error) { return hostMemory{available: 400}, nil },
	} {
		s.host = host
		sample := s.Sample(context.Background())
		if sample.Issues&CgroupUnavailable == 0 || sample.GroupCount != 0 || sample.CgroupID != ([32]byte{}) || sample.Groups[0] != (GroupMemory{}) {
			t.Fatal(name, "invalid host signal accepted", sample)
		}
	}
	s.host = func() (hostMemory, error) { return hostMemory{available: 400, identity: identity}, nil }
	s.runtime = func() (int64, error) { return 0, os.ErrNotExist }
	if sample := s.Sample(context.Background()); sample.Issues&CgroupUnavailable == 0 || sample.GroupCount != 0 {
		t.Fatal("host group without process memory", sample)
	}
}
