package resourcebudget

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"
	"runtime/metrics"
	"strconv"
	"strings"
	"time"
)

const procTextLimit = 64 << 10
const scalarLimit = 128

type diskSignal struct{ available, inodes int64 }

// Sampler does bounded synchronous reads. It creates no goroutine and never
// writes kernel controls. OS calls cannot be force-cancelled; old samples expire.
type Sampler struct {
	dataDir      string
	diskPaths    []string
	now          func() time.Time
	read         func(string, int) ([]byte, error)
	runtime      func() (int64, error)
	disk         func(string) (diskSignal, error)
	verifyCgroup func(string) ([16]byte, error)
}

func NewSampler(dataDir string) (*Sampler, error) {
	if !filepath.IsAbs(dataDir) {
		return nil, ErrInvalid
	}
	dataDir = filepath.Clean(dataDir)
	diskPaths := []string{dataDir}
	scratchDir, err := filepath.Abs(os.TempDir())
	if err != nil {
		return nil, ErrInvalid
	}
	scratchDir = filepath.Clean(scratchDir)
	if scratchDir != dataDir {
		diskPaths = append(diskPaths, scratchDir)
	}
	return &Sampler{dataDir: dataDir, diskPaths: diskPaths, now: time.Now, read: boundedText, runtime: runtimeSignal, disk: diskSpace, verifyCgroup: verifyCgroupFS}, nil
}

func boundedText(path string, limit int) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, int64(limit)+1))
	if err != nil {
		return nil, err
	}
	if len(b) > limit {
		return nil, ErrInvalid
	}
	return b, nil
}

func runtimeSignal() (int64, error) {
	samples := [2]metrics.Sample{{Name: "/memory/classes/total:bytes"}, {Name: "/memory/classes/heap/released:bytes"}}
	metrics.Read(samples[:])
	if samples[0].Value.Kind() != metrics.KindUint64 || samples[1].Value.Kind() != metrics.KindUint64 {
		return 0, ErrInvalid
	}
	total, released := samples[0].Value.Uint64(), samples[1].Value.Uint64()
	if released > total || total-released > math.MaxInt64 {
		return 0, ErrInvalid
	}
	return int64(total - released), nil
}

func scalar(b []byte, allowMax bool) (int64, bool, error) {
	text := strings.TrimSpace(string(b))
	if allowMax && text == "max" {
		return 0, false, nil
	}
	if text == "" {
		return 0, false, ErrInvalid
	}
	for _, ch := range text {
		if ch < '0' || ch > '9' {
			return 0, false, ErrInvalid
		}
	}
	v, err := strconv.ParseInt(text, 10, 64)
	if err != nil {
		return 0, false, ErrInvalid
	}
	return v, true, nil
}

func cleanAbs(path string) bool {
	return len(path) <= 4096 && filepath.IsAbs(path) && filepath.Clean(path) == path && !strings.ContainsAny(path, "\x00\n\r\t")
}

func membership(b []byte) (string, error) {
	found := ""
	for _, line := range strings.Split(string(b), "\n") {
		if !strings.HasPrefix(line, "0::") {
			continue
		}
		if found != "" {
			return "", ErrInvalid
		}
		found = strings.TrimPrefix(line, "0::")
		if !cleanAbs(found) || strings.HasSuffix(found, " (deleted)") {
			return "", ErrInvalid
		}
	}
	if found == "" {
		return "", ErrInvalid
	}
	return found, nil
}

func mountPath(text string) (string, error) {
	var out strings.Builder
	for i := 0; i < len(text); i++ {
		if text[i] != '\\' {
			out.WriteByte(text[i])
			continue
		}
		if i+3 >= len(text) {
			return "", ErrInvalid
		}
		switch text[i+1 : i+4] {
		case "040":
			out.WriteByte(' ')
		case "134":
			out.WriteByte('\\')
		default:
			return "", ErrInvalid
		}
		i += 3
	}
	p := out.String()
	if !cleanAbs(p) {
		return "", ErrInvalid
	}
	return p, nil
}

type cgroupMount struct{ root, point, leaf, identity string }

func locateCgroup(member string, b []byte) (cgroupMount, error) {
	var chosen cgroupMount
	for _, line := range strings.Split(string(b), "\n") {
		fields := strings.Fields(line)
		sep := -1
		for i, f := range fields {
			if f == "-" {
				sep = i
				break
			}
		}
		if sep < 6 || sep+3 >= len(fields) || fields[sep+1] != "cgroup2" {
			continue
		}
		root, err := mountPath(fields[3])
		if err != nil {
			return chosen, err
		}
		point, err := mountPath(fields[4])
		if err != nil {
			return chosen, err
		}
		rel, err := filepath.Rel(root, member)
		if err != nil || rel == ".." || strings.HasPrefix(rel, "../") {
			continue
		}
		leaf := filepath.Join(point, rel)
		if !cleanAbs(leaf) {
			return chosen, ErrInvalid
		}
		// Prefer the broadest visible tree so a nested bind mount cannot hide
		// readable ancestors. Equal-root aliases are safe only with same point.
		if chosen.point == "" || len(root) < len(chosen.root) {
			chosen = cgroupMount{root, point, leaf, fields[0] + "\x00" + fields[2]}
		} else if root == chosen.root && (point != chosen.point || fields[0]+"\x00"+fields[2] != chosen.identity) {
			return chosen, ErrInvalid
		}
	}
	if chosen.point == "" {
		return chosen, ErrInvalid
	}
	return chosen, nil
}

func (s *Sampler) Sample(ctx context.Context) Sample {
	result := Sample{At: s.now()}
	if ctx.Err() != nil {
		result.Issues = SampleCancelled
		return result
	}
	var err error
	result.RuntimeUsed, err = s.runtime()
	if err != nil {
		result.Issues |= RuntimeUnavailable
	}
	if ctx.Err() != nil {
		result.Issues |= SampleCancelled
		return result
	}
	result.DiskAvailable, result.InodesAvailable, err = s.DiskHeadroom(ctx, "")
	if err != nil {
		if ctx.Err() != nil {
			result.Issues |= SampleCancelled
			return result
		}
		result.Issues |= DiskUnavailable
	}
	if ctx.Err() != nil {
		result.Issues |= SampleCancelled
		return result
	}
	if err := s.collectCgroup(ctx, &result); err != nil {
		result.Issues |= CgroupUnavailable
		result.GroupCount = 0
		result.CgroupID = [32]byte{}
	}
	if ctx.Err() != nil {
		result.Issues |= SampleCancelled
	}
	return result
}

// DiskHeadroom reports the lowest available bytes and inodes across the
// configured data/temp filesystems and one operation-specific output directory.
// An empty extra path checks only the configured filesystems.
func (s *Sampler) DiskHeadroom(ctx context.Context, extraPath string) (available, inodes int64, err error) {
	if s == nil || ctx == nil || s.disk == nil {
		return 0, 0, ErrInvalid
	}
	paths := s.diskPaths
	if len(paths) == 0 {
		paths = []string{s.dataDir}
	}
	if extraPath != "" {
		if !cleanAbs(extraPath) {
			return 0, 0, ErrInvalid
		}
		found := false
		for _, path := range paths {
			if path == extraPath {
				found = true
				break
			}
		}
		if !found {
			paths = append(append([]string(nil), paths...), extraPath)
		}
	}
	haveDisk := false
	for _, path := range paths {
		if err := ctx.Err(); err != nil {
			return 0, 0, err
		}
		disk, err := s.disk(path)
		if err != nil {
			return 0, 0, err
		}
		if !haveDisk || disk.available < available {
			available = disk.available
		}
		if !haveDisk || disk.inodes < inodes {
			inodes = disk.inodes
		}
		haveDisk = true
	}
	if !haveDisk {
		return 0, 0, ErrInvalid
	}
	return available, inodes, ctx.Err()
}

func (s *Sampler) collectCgroup(ctx context.Context, result *Sample) error {
	members, err := s.read("/proc/self/cgroup", procTextLimit)
	if err != nil {
		return err
	}
	member, err := membership(members)
	if err != nil {
		return err
	}
	mounts, err := s.read("/proc/self/mountinfo", procTextLimit)
	if err != nil {
		return err
	}
	mount, err := locateCgroup(member, mounts)
	if err != nil {
		return err
	}
	identity, err := s.verifyCgroup(mount.leaf)
	if err != nil {
		return err
	}
	for path := mount.leaf; ; path = filepath.Dir(path) {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if result.GroupCount == MaxCgroupDepth {
			return ErrInvalid
		}
		current, e1 := s.read(filepath.Join(path, "memory.current"), scalarLimit)
		max, e2 := s.read(filepath.Join(path, "memory.max"), scalarLimit)
		high, e3 := s.read(filepath.Join(path, "memory.high"), scalarLimit)
		// The true kernel root has no per-group memory controls. A non-leaf
		// visible boundary with all three absent adds no readable root signal.
		// Missing controls on the leaf/intermediate nodes always fail closed.
		rootAbsent := path == mount.point && path != mount.leaf && errors.Is(e1, os.ErrNotExist) && errors.Is(e2, os.ErrNotExist) && errors.Is(e3, os.ErrNotExist)
		if !rootAbsent {
			if e1 != nil || e2 != nil || e3 != nil {
				return errors.Join(e1, e2, e3)
			}
			g := GroupMemory{}
			g.Current, _, err = scalar(current, false)
			if err != nil {
				return err
			}
			g.Max, g.MaxLimited, err = scalar(max, true)
			if err != nil {
				return err
			}
			g.High, g.HighLimited, err = scalar(high, true)
			if err != nil {
				return err
			}
			result.Groups[result.GroupCount] = g
			result.GroupCount++
		}
		if path == mount.point {
			break
		}
		parent := filepath.Dir(path)
		rel, err := filepath.Rel(mount.point, parent)
		if err != nil || parent == path || rel == ".." || strings.HasPrefix(rel, "../") {
			return ErrInvalid
		}
	}
	if result.GroupCount == 0 {
		return ErrInvalid
	}
	// Detect movement/remount during collection. This is a sampled view, not
	// an atomic kernel transaction or knowledge of namespace-hidden parents.
	afterIdentity, err := s.verifyCgroup(mount.leaf)
	if err != nil || afterIdentity != identity {
		return ErrInvalid
	}
	after, err := s.read("/proc/self/cgroup", procTextLimit)
	if err != nil || !bytes.Equal(after, members) {
		return ErrInvalid
	}
	after, err = s.read("/proc/self/mountinfo", procTextLimit)
	if err != nil || !bytes.Equal(after, mounts) {
		return ErrInvalid
	}
	result.CgroupID = sha256.Sum256([]byte(member + "\x00" + mount.root + "\x00" + mount.point + "\x00" + mount.identity + "\x00" + string(identity[:])))
	return nil
}
