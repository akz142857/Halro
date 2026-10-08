//go:build !linux && !darwin

package replication

func renamePortableReceivedOpaque(from, to string) error { return ErrMemberAuthority }
