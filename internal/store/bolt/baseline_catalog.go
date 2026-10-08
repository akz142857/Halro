package bolt

import (
	"crypto/sha256"
	"encoding/binary"
	"slices"
)

// MetadataBaselineClass uses the actual journal catalog, failing closed for
// unknown keys/buckets. It classifies only; it authenticates no caller's DB.
func MetadataBaselineClass(path []string, key []byte) (string, error) {
	c, err := classify(path, key)
	if err != nil {
		return "", err
	}
	return c.String(), nil
}

// MetadataBaselineCatalogDigest binds the exact schema and both fixed class
// tables. These tables contain compile-time schema names, not historical rows.
func MetadataBaselineCatalogDigest() [32]byte {
	b := []byte("halro:metadata:baseline-catalog:v1\x00")
	b = binary.BigEndian.AppendUint64(b, CurrentSchemaVersion())
	for _, table := range []map[string]journalClass{bucketClasses, metaKeyClasses} {
		names := make([]string, 0, len(table))
		for name := range table {
			names = append(names, name)
		}
		slices.Sort(names)
		b = binary.BigEndian.AppendUint16(b, uint16(len(names)))
		for _, name := range names {
			b = binary.BigEndian.AppendUint16(b, uint16(len(name)))
			b = append(b, name...)
			b = append(b, byte(table[name]))
		}
	}
	return sha256.Sum256(b)
}
