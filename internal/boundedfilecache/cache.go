// Package boundedfilecache requests finite captured-descriptor cache windows.
// Successful advice is not evidence of a RAM bound or persistent retirement.
package boundedfilecache

import (
	"errors"
	"io"
	"os"
)

const WindowBytes int64 = 32 << 20

// Writer is exclusively owned by the caller holding File. Advance is called
// only after complete writes; it syncs before advising fully completed pages.
type Writer struct {
	File *os.File
	end  int64
}

func (w *Writer) Advance(end int64, final bool) error {
	if w.File == nil || end < w.end {
		return os.ErrInvalid
	}
	if !final && end-w.end < WindowBytes {
		return nil
	}
	if err := w.File.Sync(); err != nil {
		return err
	}
	complete := end / int64(os.Getpagesize()) * int64(os.Getpagesize())
	if complete > w.end {
		if err := advise(w.File, w.end, complete-w.end); err != nil {
			return err
		}
		w.end = complete
	}
	return nil
}

// Reader keeps seek semantics while advising completed sequential windows on
// the actual borrowed descriptor. It does not close, refund or qualify File.
type Reader struct {
	file              *os.File
	section           *io.SectionReader
	position, retired int64
	err               error
}

func NewReader(file *os.File, length int64) *Reader {
	return &Reader{file: file, section: io.NewSectionReader(file, 0, length)}
}
func (r *Reader) Read(p []byte) (int, error) {
	if r.err != nil {
		return 0, r.err
	}
	n, err := r.section.Read(p)
	r.position += int64(n)
	if r.position-r.retired >= WindowBytes || errors.Is(err, io.EOF) {
		complete := r.position / int64(os.Getpagesize()) * int64(os.Getpagesize())
		if complete > r.retired {
			if cacheErr := advise(r.file, r.retired, complete-r.retired); cacheErr != nil {
				r.err = cacheErr
				return n, errors.Join(err, cacheErr)
			}
			r.retired = complete
		}
	}
	return n, err
}
func (r *Reader) Seek(offset int64, whence int) (int64, error) {
	if r.err != nil {
		return 0, r.err
	}
	p, err := r.section.Seek(offset, whence)
	if err == nil {
		r.position = p
		if p < r.retired {
			r.retired = p / int64(os.Getpagesize()) * int64(os.Getpagesize())
		}
	}
	return p, err
}
