//go:build !linux && !darwin

package resourcebudget

func diskSpace(string) (diskSignal, error)    { return diskSignal{}, ErrInvalid }
func verifyCgroupFS(string) ([16]byte, error) { return [16]byte{}, ErrInvalid }
