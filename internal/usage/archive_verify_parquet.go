package usage

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/klauspost/compress/snappy"
	"github.com/klauspost/compress/zstd"
	"github.com/parquet-go/parquet-go"
	"github.com/parquet-go/parquet-go/encoding/thrift"
	"github.com/parquet-go/parquet-go/format"
)

// Inspect compact Thrift framing before the library allocates declared lists,
// strings or value buffers. BytesReader aliases the already capped input.
func archiveThriftFrame(raw []byte, maxNodes int) (int, error) {
	reader := (&thrift.CompactProtocol{}).NewReaderFromBytes(raw)
	nodes := 0
	var value func(thrift.Type, int, bool) error
	value = func(kind thrift.Type, depth int, field bool) error {
		nodes++
		if nodes > maxNodes || depth > 16 {
			return ErrDiskUsageLimit
		}
		var err error
		switch kind {
		case thrift.STRUCT:
			for {
				f, e := reader.ReadField()
				if e != nil {
					return e
				}
				if f.Type == thrift.STOP {
					break
				}
				if e := value(f.Type, depth+1, true); e != nil {
					return e
				}
			}
		case thrift.TRUE, thrift.FALSE:
			if !field {
				_, err = reader.ReadBool()
			}
		case thrift.I8:
			_, err = reader.ReadInt8()
		case thrift.I16:
			_, err = reader.ReadInt16()
		case thrift.I32:
			_, err = reader.ReadInt32()
		case thrift.I64:
			_, err = reader.ReadInt64()
		case thrift.DOUBLE:
			_, err = reader.ReadFloat64()
		case thrift.BINARY:
			_, err = reader.ReadBytes()
		case thrift.LIST, thrift.SET:
			list, e := reader.ReadList()
			if e != nil {
				return e
			}
			if list.Size < 0 || int64(list.Size) > int64(maxNodes-nodes) {
				return ErrDiskUsageLimit
			}
			for i := int32(0); i < list.Size; i++ {
				if e := value(list.Type, depth+1, false); e != nil {
					return e
				}
			}
		case thrift.MAP:
			m, e := reader.ReadMap()
			if e != nil {
				return e
			}
			if m.Size < 0 || int64(m.Size)*2 > int64(maxNodes-nodes) {
				return ErrDiskUsageLimit
			}
			for i := int32(0); i < m.Size; i++ {
				if e := value(m.Key, depth+1, false); e != nil {
					return e
				}
				if e := value(m.Value, depth+1, false); e != nil {
					return e
				}
			}
		default:
			return ErrDiskUsageCorrupt
		}
		return err
	}
	if err := value(thrift.STRUCT, 0, false); err != nil {
		return 0, err
	}
	return reader.BytesRead(), nil
}

type archiveVerifyReaderAt struct {
	ctx    context.Context
	file   io.ReaderAt
	budget *archiveVerifyReadBudget
}

func (r *archiveVerifyReaderAt) ReadAt(buffer []byte, offset int64) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	if err := r.budget.reserve(int64(len(buffer))); err != nil {
		return 0, err
	}
	return r.file.ReadAt(buffer, offset)
}

// The input is a checksum-validated, privately owned immutable spool. None of
// the untrusted library metadata/page decoders run before this preflight.
func archiveParquetPreflight(ctx context.Context, f *os.File, size int64, budget *archiveVerifyReadBudget) (*parquet.File, error) {
	if size < 12 {
		return nil, ErrDiskUsageCorrupt
	}
	reader := &archiveVerifyReaderAt{ctx: ctx, file: f, budget: budget}
	var tail [8]byte
	if _, err := reader.ReadAt(tail[:], size-8); err != nil {
		return nil, err
	}
	footerSize := int64(binary.LittleEndian.Uint32(tail[:4]))
	if string(tail[4:]) != "PAR1" || footerSize < 1 || footerSize > 1<<20 || footerSize > size-12 {
		return nil, ErrDiskUsageLimit
	}
	footer := make([]byte, footerSize)
	if _, err := reader.ReadAt(footer, size-8-footerSize); err != nil {
		return nil, err
	}
	n, err := archiveThriftFrame(footer, 65536)
	if err != nil {
		return nil, err
	}
	if n != len(footer) {
		return nil, ErrDiskUsageCorrupt
	}
	file, err := parquet.OpenFile(reader, size, parquet.SkipPageIndex(true), parquet.SkipBloomFilters(true), parquet.ReadBufferSize(32<<10), parquet.FileReadMode(parquet.ReadModeSync))
	if err != nil {
		return nil, err
	}
	meta := file.Metadata()
	if len(meta.Schema) > 128 || len(meta.RowGroups) > 128 {
		return nil, ErrDiskUsageLimit
	}
	var pageCount int
	var uncompressed int64
	for _, group := range meta.RowGroups {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if len(group.Columns) > 64 || group.NumRows < 1 || group.NumRows > 1<<20 {
			return nil, ErrDiskUsageLimit
		}
		for _, column := range group.Columns {
			m := column.MetaData
			start := m.DataPageOffset
			if m.DictionaryPageOffset > 0 && m.DictionaryPageOffset < start {
				start = m.DictionaryPageOffset
			}
			if start < 4 || m.TotalCompressedSize < 1 || m.TotalCompressedSize > size-start || start+m.TotalCompressedSize > size-8-footerSize {
				return nil, ErrDiskUsageCorrupt
			}
			end := start + m.TotalCompressedSize
			for offset := start; offset < end; {
				if err := ctx.Err(); err != nil {
					return nil, err
				}
				pageCount++
				if pageCount > 65536 {
					return nil, ErrDiskUsageLimit
				}
				raw := make([]byte, min(int64(64<<10), end-offset))
				if _, err := reader.ReadAt(raw, offset); err != nil {
					return nil, err
				}
				length, err := archiveThriftFrame(raw, 1024)
				if err != nil {
					return nil, err
				}
				var header format.PageHeader
				if err := thrift.NewDecoder((&thrift.CompactProtocol{}).NewReaderFromBytes(raw[:length])).Decode(&header); err != nil {
					return nil, err
				}
				if header.CompressedPageSize < 0 || header.CompressedPageSize > 8<<20 || header.UncompressedPageSize < 0 || header.UncompressedPageSize > 8<<20 || int64(header.CompressedPageSize) > end-offset-int64(length) {
					return nil, ErrDiskUsageLimit
				}
				uncompressed += int64(header.UncompressedPageSize)
				if uncompressed > 256<<20 {
					return nil, ErrDiskUsageLimit
				}
				if h := header.DictionaryPageHeader; h.Valid && (h.V.NumValues < 0 || h.V.NumValues > 1<<20) {
					return nil, ErrDiskUsageLimit
				}
				if h := header.DataPageHeader; h.Valid && (h.V.NumValues < 0 || h.V.NumValues > 1<<20) {
					return nil, ErrDiskUsageLimit
				}
				if h := header.DataPageHeaderV2; h.Valid && (h.V.NumValues < 0 || h.V.NumValues > 1<<20 || h.V.NumRows < 0 || h.V.NumRows > 1<<20 || h.V.NumNulls < 0 || h.V.NumNulls > h.V.NumValues || h.V.DefinitionLevelsByteLength < 0 || h.V.RepetitionLevelsByteLength < 0 || int64(h.V.DefinitionLevelsByteLength)+int64(h.V.RepetitionLevelsByteLength) > int64(header.UncompressedPageSize)) {
					return nil, ErrDiskUsageLimit
				}
				if header.Type != format.DataPage && header.Type != format.DataPageV2 && header.Type != format.DictionaryPage {
					return nil, ErrDiskUsageCorrupt
				}
				compressed := make([]byte, int(header.CompressedPageSize))
				if _, err := reader.ReadAt(compressed, offset+int64(length)); err != nil {
					return nil, err
				}
				if err := archiveValidateCompressedPage(m.Codec, header, compressed); err != nil {
					return nil, err
				}
				offset += int64(length) + int64(header.CompressedPageSize)
			}
		}
	}
	return file, nil
}

func archiveReadParquet(ctx context.Context, f *os.File, size int64, budget *archiveVerifyReadBudget, visit func(parquetAttempt) error) (err error) {
	// Constructors can panic on unsupported schemas; turn data errors into a
	// refused job, preserving all publication/retention authority.
	defer func() {
		if recover() != nil {
			err = errors.Join(err, ErrDiskUsageCorrupt)
		}
	}()
	file, err := archiveParquetPreflight(ctx, f, size, budget)
	if err != nil {
		return err
	}
	reader := parquet.NewGenericReader[parquetAttempt](file)
	defer func() { err = errors.Join(err, reader.Close()) }()
	rows := make([]parquetAttempt, 16)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		n, readErr := reader.Read(rows)
		for i := 0; i < n; i++ {
			if err := visit(rows[i]); err != nil {
				return err
			}
			rows[i] = parquetAttempt{}
		}
		if errors.Is(readErr, io.EOF) {
			return nil
		}
		if readErr != nil {
			return readErr
		}
		if n == 0 {
			return io.ErrNoProgress
		}
	}
}

func archiveValidateCompressedPage(codec format.CompressionCodec, header format.PageHeader, raw []byte) error {
	expected := int(header.UncompressedPageSize)
	if expected < 0 || expected > 8<<20 || len(raw) > 8<<20 {
		return ErrDiskUsageLimit
	}
	if h := header.DataPageHeaderV2; h.Valid {
		prefix := int(h.V.DefinitionLevelsByteLength) + int(h.V.RepetitionLevelsByteLength)
		if prefix < 0 || prefix > len(raw) || prefix > expected {
			return ErrDiskUsageCorrupt
		}
		expected -= prefix
		raw = raw[prefix:]
		if h.V.IsCompressed.Valid && !h.V.IsCompressed.V {
			codec = format.Uncompressed
		}
	}
	switch codec {
	case format.Uncompressed:
		if len(raw) != expected {
			return ErrDiskUsageCorrupt
		}
	case format.Snappy:
		length, err := snappy.DecodedLen(raw)
		if err != nil {
			return err
		}
		if length != expected {
			return ErrDiskUsageLimit
		}
		decoded, err := snappy.Decode(make([]byte, 0, expected), raw)
		if err != nil {
			return err
		}
		if len(decoded) != expected {
			return ErrDiskUsageCorrupt
		}
	case format.Zstd:
		decoder, err := zstd.NewReader(nil, zstd.WithDecoderConcurrency(1), zstd.WithDecoderMaxMemory(16<<20), zstd.WithDecoderMaxWindow(8<<20), zstd.WithDecodeAllCapLimit(true))
		if err != nil {
			return err
		}
		defer decoder.Close()
		decoded, err := decoder.DecodeAll(raw, make([]byte, 0, expected))
		if err != nil {
			return err
		}
		if len(decoded) != expected {
			return ErrDiskUsageCorrupt
		}
	default:
		return fmt.Errorf("%w: unsupported archive Parquet codec %d", ErrDiskUsageCorrupt, codec)
	}
	return nil
}
