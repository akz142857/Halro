package usage

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

const archiveManifestValueLimit = 64 << 10
const archiveManifestBytesLimit int64 = 1 << 30

// ArchivePosition is constant-size; it does not collect the file catalog.
// It is derived archive metadata, not permission to trust a Usage projection.
type ArchivePosition struct {
	SchemaVersion  int
	LastSequence   uint64
	CompletePrefix bool
	Files          uint64
	CatalogVersion int
	CatalogHead    ArchivePageRef
	Retention      ArchiveDateRetention
}

type archiveManifestInput struct {
	file *os.File
	info os.FileInfo
	path string
}

func openArchiveManifest(root string) (*archiveManifestInput, error) {
	path := filepath.Join(root, "manifest.json")
	named, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !named.Mode().IsRegular() {
		return nil, ErrDiskUsageCorrupt
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > archiveManifestBytesLimit {
		f.Close()
		return nil, ErrDiskUsageLimit
	}
	return &archiveManifestInput{file: f, info: info, path: path}, nil
}
func (m *archiveManifestInput) check() error {
	current, err := m.file.Stat()
	if err != nil {
		return err
	}
	named, err := os.Lstat(m.path)
	if err != nil {
		return err
	}
	if !os.SameFile(m.info, current) || !os.SameFile(m.info, named) || m.info.Size() != current.Size() || m.info.Size() != named.Size() || m.info.ModTime() != current.ModTime() || m.info.ModTime() != named.ModTime() {
		return fmt.Errorf("%w: archive catalog changed", ErrDiskUsageCorrupt)
	}
	return nil
}
func (m *archiveManifestInput) walk(ctx context.Context, visit func(ManifestFile) error) (ArchivePosition, [32]byte, error) {
	h := sha256.New()
	position, err := readArchiveManifest(ctx, io.TeeReader(io.NewSectionReader(m.file, 0, m.info.Size()), h), visit)
	if err == nil && position.CatalogVersion == 1 && m.info.Size() > archiveManifestValueLimit {
		err = ErrDiskUsageLimit
	}
	if err == nil {
		err = m.check()
	}
	if err != nil {
		return ArchivePosition{}, [32]byte{}, err
	}
	var digest [32]byte
	copy(digest[:], h.Sum(nil))
	return position, digest, nil
}
func (e *Exporter) ReadArchivePosition(ctx context.Context) (result ArchivePosition, err error) {
	finish, err := e.beginArchiveRead(ctx)
	if err != nil {
		return result, err
	}
	defer func() {
		err = errors.Join(err, finish())
		if err != nil {
			result = ArchivePosition{}
		}
	}()
	return e.readArchivePositionHeld(ctx)
}

func (e *Exporter) readArchivePositionHeld(ctx context.Context) (result ArchivePosition, err error) {
	m, err := openArchiveManifest(e.root)
	if err != nil {
		return ArchivePosition{}, err
	}
	defer func() {
		err = errors.Join(err, m.file.Close(), ctx.Err())
		if err != nil {
			result = ArchivePosition{}
		}
	}()
	result, _, err = m.walk(ctx, nil)
	return result, err
}

// Individual values are bounded before JSON decoding, including unknown fields.
// Callbacks are tentative until the complete document and identity validate.
func readArchiveManifest(ctx context.Context, source io.Reader, visit func(ManifestFile) error) (ArchivePosition, error) {
	input := bufio.NewReaderSize(source, archiveManifestValueLimit)
	if err := archiveExpect(input, '{'); err != nil {
		return ArchivePosition{}, err
	}
	var result ArchivePosition
	var fields uint8
	var catalog *archiveCatalogRoot
	var retention *ArchiveDateRetention
	for {
		if err := ctx.Err(); err != nil {
			return ArchivePosition{}, err
		}
		c, err := archiveNonSpace(input)
		if err != nil {
			return ArchivePosition{}, err
		}
		if c == '}' {
			break
		}
		if err := input.UnreadByte(); err != nil {
			return ArchivePosition{}, err
		}
		raw, err := archiveValue(input)
		if err != nil {
			return ArchivePosition{}, err
		}
		var name string
		if len(raw) == 0 || raw[0] != '"' || json.Unmarshal(raw, &name) != nil {
			return ArchivePosition{}, ErrDiskUsageCorrupt
		}
		if err := archiveExpect(input, ':'); err != nil {
			return ArchivePosition{}, err
		}
		bit := uint8(0)
		switch name {
		case "schema_version":
			bit = 1
		case "last_sequence":
			bit = 2
		case "files":
			bit = 4
		case "complete_prefix":
			bit = 8
		case "catalog":
			bit = 16
		case "retention":
			bit = 32
		}
		if bit != 0 && fields&bit != 0 {
			return ArchivePosition{}, ErrDiskUsageCorrupt
		}
		fields |= bit
		if name == "files" {
			c, err := archiveNonSpace(input)
			if err != nil {
				return ArchivePosition{}, err
			}
			if c == 'n' {
				input.UnreadByte()
				raw, err := archiveValue(input)
				if err != nil || string(raw) != "null" {
					return ArchivePosition{}, ErrDiskUsageCorrupt
				}
			} else if c == '[' {
				c, err := archiveNonSpace(input)
				if err != nil {
					return ArchivePosition{}, err
				}
				if c != ']' {
					input.UnreadByte()
					for {
						if err := ctx.Err(); err != nil {
							return ArchivePosition{}, err
						}
						raw, err := archiveValue(input)
						if err != nil {
							return ArchivePosition{}, err
						}
						var entry ManifestFile
						if len(raw) == 0 || raw[0] != '{' || json.Unmarshal(raw, &entry) != nil {
							return ArchivePosition{}, ErrDiskUsageCorrupt
						}
						if result.Files == ^uint64(0) {
							return ArchivePosition{}, ErrDiskUsageLimit
						}
						result.Files++
						if visit != nil {
							if err := visit(entry); err != nil {
								return ArchivePosition{}, err
							}
						}
						c, err := archiveNonSpace(input)
						if err != nil {
							return ArchivePosition{}, err
						}
						if c == ']' {
							break
						}
						if c != ',' {
							return ArchivePosition{}, ErrDiskUsageCorrupt
						}
						c, err = archiveNonSpace(input)
						if err != nil || c == ']' {
							return ArchivePosition{}, ErrDiskUsageCorrupt
						}
						input.UnreadByte()
					}
				}
			} else {
				return ArchivePosition{}, ErrDiskUsageCorrupt
			}
		} else {
			raw, err := archiveValue(input)
			if err != nil || !json.Valid(raw) {
				return ArchivePosition{}, errors.Join(err, ErrDiskUsageCorrupt)
			}
			switch name {
			case "schema_version":
				err = json.Unmarshal(raw, &result.SchemaVersion)
			case "last_sequence":
				err = json.Unmarshal(raw, &result.LastSequence)
			case "complete_prefix":
				err = json.Unmarshal(raw, &result.CompletePrefix)
			case "catalog":
				err = json.Unmarshal(raw, &catalog)
			case "retention":
				retention, err = decodeArchiveDateRetention(raw)
			}
			if err != nil {
				return ArchivePosition{}, ErrDiskUsageCorrupt
			}
		}
		c, err = archiveNonSpace(input)
		if err != nil {
			return ArchivePosition{}, err
		}
		if c == '}' {
			break
		}
		if c != ',' {
			return ArchivePosition{}, ErrDiskUsageCorrupt
		}
		c, err = archiveNonSpace(input)
		if err != nil || c == '}' {
			return ArchivePosition{}, ErrDiskUsageCorrupt
		}
		input.UnreadByte()
	}
	if fields&7 != 7 {
		return ArchivePosition{}, ErrDiskUsageCorrupt
	}
	if catalog != nil {
		if result.SchemaVersion != archiveCatalogSchema && result.SchemaVersion != archiveDateCatalogSchema || catalog.Version != 1 || result.Files != 0 || !catalog.Head.valid() {
			return ArchivePosition{}, ErrDiskUsageCorrupt
		}
		result.CatalogVersion = catalog.Version
		result.CatalogHead = catalog.Head
		result.Files = catalog.Head.Files
	} else if !supportedManifestSchema(result.SchemaVersion) {
		return ArchivePosition{}, fmt.Errorf("usage manifest schema version %d is not supported", result.SchemaVersion)
	}
	if result.SchemaVersion == archiveDateCatalogSchema {
		if retention == nil || !retention.valid(result.LastSequence) || result.CompletePrefix || fields&8 == 0 {
			return ArchivePosition{}, ErrDiskUsageCorrupt
		}
		result.Retention = *retention
	} else if fields&32 != 0 {
		// Even null must not smuggle date semantics into an older format.
		return ArchivePosition{}, ErrDiskUsageCorrupt
	}
	if _, err := archiveNonSpace(input); !errors.Is(err, io.EOF) {
		return ArchivePosition{}, errors.Join(err, ErrDiskUsageCorrupt)
	}
	return result, ctx.Err()
}

func archiveNonSpace(input *bufio.Reader) (byte, error) {
	for {
		c, err := input.ReadByte()
		if err != nil {
			return 0, err
		}
		if c != ' ' && c != '\n' && c != '\r' && c != '\t' {
			return c, nil
		}
	}
}

func archiveExpect(input *bufio.Reader, want byte) error {
	c, err := archiveNonSpace(input)
	if err != nil {
		return fmt.Errorf("%w: incomplete manifest: %v", ErrDiskUsageCorrupt, err)
	}
	if c != want {
		return fmt.Errorf("%w: expected manifest delimiter %q", ErrDiskUsageCorrupt, want)
	}
	return nil
}

// archiveValue bounds a JSON value before decoding. It deliberately handles
// only framing; json.Unmarshal/Valid still performs JSON grammar validation.
func archiveValue(input *bufio.Reader) ([]byte, error) {
	return archiveSizedValue(input, archiveManifestValueLimit)
}
func archiveSizedValue(input *bufio.Reader, limit int) ([]byte, error) {
	c, err := archiveNonSpace(input)
	if err != nil {
		return nil, err
	}
	value := []byte{c}
	depth := 0
	inString, escaped := c == '"', false
	if c == '{' || c == '[' {
		depth = 1
	}
	structured := inString || depth > 0
	for {
		if !structured {
			next, err := input.Peek(1)
			if errors.Is(err, io.EOF) {
				return value, nil
			}
			if err != nil {
				return nil, err
			}
			n := next[0]
			if n == ',' || n == '}' || n == ']' || n == ' ' || n == '\n' || n == '\r' || n == '\t' {
				return value, nil
			}
		}
		if len(value) >= limit {
			return nil, fmt.Errorf("%w: manifest value exceeds %d bytes", ErrDiskUsageCorrupt, limit)
		}
		c, err := input.ReadByte()
		if err != nil {
			return nil, fmt.Errorf("%w: incomplete manifest value", ErrDiskUsageCorrupt)
		}
		value = append(value, c)
		if inString {
			if escaped {
				escaped = false
			} else if c == '\\' {
				escaped = true
			} else if c == '"' {
				inString = false
				if depth == 0 {
					return value, nil
				}
			}
		} else {
			switch c {
			case '"':
				inString = true
			case '{', '[':
				depth++
			case '}', ']':
				depth--
				if depth == 0 {
					return value, nil
				}
			}
		}
	}
}
