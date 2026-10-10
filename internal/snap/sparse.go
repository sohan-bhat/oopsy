package snap

import (
	"bytes"
	"io"
	"os"
)

// Sparse files, like Docker's disk image, claim a large size while most of
// it is holes that take no disk. Oopsy counts what files really take on
// disk, and copies keep the holes: data regions are copied, runs of zero
// blocks become holes again.

const holeBlock = 64 << 10

var zeroBlock = make([]byte, holeBlock)

// holeWriter writes to f, turning whole blocks of zeros into holes by
// seeking past them. finish sets the final length.
type holeWriter struct {
	f   *os.File
	off int64
}

func (w *holeWriter) Write(p []byte) (int, error) {
	n := 0
	for len(p) > 0 {
		chunk := p
		if len(chunk) > holeBlock {
			chunk = chunk[:holeBlock]
		}
		if len(chunk) == holeBlock && bytes.Equal(chunk, zeroBlock) {
			w.off += int64(len(chunk))
		} else {
			if _, err := w.f.Seek(w.off, io.SeekStart); err != nil {
				return n, err
			}
			m, err := w.f.Write(chunk)
			w.off += int64(m)
			if err != nil {
				return n + m, err
			}
		}
		n += len(chunk)
		p = p[len(chunk):]
	}
	return n, nil
}

func (w *holeWriter) finish() error { return w.f.Truncate(w.off) }

// copySparse copies in to out keeping holes. It reads only the data
// regions where the system can say where they are.
func copySparse(in, out *os.File, size int64) error {
	markSparse(in, out)
	w := &holeWriter{f: out}
	if regions, ok := dataRegions(in, size); ok {
		buf := make([]byte, 1<<20)
		for _, r := range regions {
			w.off = r[0]
			if _, err := in.Seek(r[0], io.SeekStart); err != nil {
				return err
			}
			if _, err := io.CopyBuffer(w, io.LimitReader(in, r[1]-r[0]), buf); err != nil {
				return err
			}
		}
		w.off = size
		return w.finish()
	}
	if _, err := io.CopyBuffer(w, in, make([]byte, 1<<20)); err != nil {
		return err
	}
	return w.finish()
}

// isSparse reports a file that takes much less disk than its size.
func isSparse(p string, fi os.FileInfo) bool {
	return fi.Size() > holeBlock && allocBytes(p, fi) < fi.Size()/2
}
