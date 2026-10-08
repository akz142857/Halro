package usage

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Catalog-only IO/allocation evidence; this does not measure Parquet working
// heap, RSS, shared fee calibration or a complete Linux/runtime capacity gate.
func BenchmarkArchiveManifestStreaming(b *testing.B) {
	for _, count := range []int{100, 10000} {
		b.Run(fmt.Sprint(count), func(b *testing.B) {
			root := b.TempDir()
			file, err := os.Create(filepath.Join(root, "manifest.json"))
			if err != nil {
				b.Fatal(err)
			}
			if _, err := fmt.Fprintf(file, `{"schema_version":8,"last_sequence":%d,"files":[`, count); err != nil {
				b.Fatal(err)
			}
			encoder := json.NewEncoder(file)
			for i := 1; i <= count; i++ {
				if i > 1 {
					if _, err := file.WriteString(","); err != nil {
						b.Fatal(err)
					}
				}
				if err := encoder.Encode(ManifestFile{SchemaVersion: 8, Path: fmt.Sprintf("date=2026-10-03/usage-%020d.ndjson", i), Date: "2026-10-03", MinSequence: uint64(i), MaxSequence: uint64(i), Records: 1, Format: FormatNDJSON, SHA256: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"}); err != nil {
					b.Fatal(err)
				}
			}
			if _, err := file.WriteString("]}"); err != nil {
				b.Fatal(err)
			}
			if err := file.Close(); err != nil {
				b.Fatal(err)
			}
			info, err := os.Stat(filepath.Join(root, "manifest.json"))
			if err != nil {
				b.Fatal(err)
			}
			e := &Exporter{root: root, format: FormatNDJSON}
			b.ReportAllocs()
			b.SetBytes(info.Size())
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				position, err := e.ReadArchivePosition(context.Background())
				if err != nil || position.Files != uint64(count) {
					b.Fatal(position, err)
				}
			}
		})
	}
}

func BenchmarkArchiveCatalogPosition(b *testing.B) {
	for _, count := range []int{100, 10000} {
		b.Run(fmt.Sprint(count), func(b *testing.B) {
			e, err := NewExporter(b.TempDir())
			if err != nil {
				b.Fatal(err)
			}
			manifest := Manifest{SchemaVersion: 8, LastSequence: uint64(count)}
			for i := 1; i <= count; i++ {
				manifest.Files = append(manifest.Files, syntheticArchiveEntry(uint64(i)))
			}
			if err := e.commitManifest(manifest); err != nil {
				b.Fatal(err)
			}
			if _, err := e.MigrateArchiveCatalog(context.Background(), time.Minute); err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				position, err := e.ReadArchivePosition(context.Background())
				if err != nil || position.Files != uint64(count) || position.CatalogVersion != 1 {
					b.Fatal(position, err)
				}
			}
		})
	}
}
