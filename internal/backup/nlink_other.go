//go:build !aix && !darwin && !dragonfly && !freebsd && !linux && !netbsd && !openbsd && !solaris

package backup

import "os"

func hasMultipleLinks(os.FileInfo) bool {
	return false
}
