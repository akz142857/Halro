//go:build !linux

package authroot

import "os"

func configurePackAccess(*os.File) error            { return errCacheUnsupported }
func discardPackCache(*os.File, int64, int64) error { return errCacheUnsupported }
