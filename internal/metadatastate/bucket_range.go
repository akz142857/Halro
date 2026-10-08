package metadatastate

// BucketRange returns codec1's [lo, hi) for a bucket marker and all descendants.
// Every path component ends in 00 00, while an embedded zero is 00 ff; increasing
// the final terminator byte therefore excludes siblings even for binary names.
// The caller must prove the bucket exists and enforce bbolt's shared key/bucket
// namespace before authorizing DeleteRange. This does not validate a native op
// or grant permission to delete any particular state.
func BucketRange(path [][]byte) (lo, hi []byte, err error) {
	marker, err := RowKey(path, nil, true)
	if err != nil {
		return nil, nil, err
	}
	lo = marker
	hi = append([]byte(nil), marker[:len(marker)-1]...)
	hi[len(hi)-1]++ // final component terminator is always zero
	return lo, hi, nil
}
