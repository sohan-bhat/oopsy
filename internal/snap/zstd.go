package snap

import (
	"bufio"
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/klauspost/compress/zstd"
)

// Copies on disks without copy on write (ext4, and any second drive store)
// are compressed with zstd. Files that are already compressed (images,
// video, archives) are stored as they are, since zstd would only spend time
// on them. A stored file is a zstd frame exactly when it starts with the zstd
// magic number; a raw file that happens to start with it is compressed
// anyway, so reading never guesses wrong.

var zstdMagic = []byte{0x28, 0xb5, 0x2f, 0xfd}

var (
	encOnce sync.Once
	enc     *zstd.Encoder
	decOnce sync.Once
	dec     *zstd.Decoder
)

func encoder() *zstd.Encoder {
	encOnce.Do(func() {
		enc, _ = zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedDefault), zstd.WithEncoderConcurrency(1))
	})
	return enc
}

func decoder() *zstd.Decoder {
	decOnce.Do(func() { dec, _ = zstd.NewReader(nil, zstd.WithDecoderConcurrency(1)) })
	return dec
}

// compressedExt lists formats that are already compressed.
var compressedExt = map[string]bool{
	".zip": true, ".gz": true, ".tgz": true, ".bz2": true, ".xz": true, ".zst": true, ".7z": true, ".rar": true,
	".lz4": true, ".br": true, ".jar": true, ".whl": true, ".apk": true, ".nupkg": true, ".docx": true, ".xlsx": true,
	".pptx": true, ".odt": true, ".jpg": true, ".jpeg": true, ".png": true, ".gif": true, ".webp": true, ".heic": true,
	".avif": true, ".mp4": true, ".mov": true, ".mkv": true, ".webm": true, ".mp3": true, ".aac": true, ".ogg": true,
	".opus": true, ".flac": true, ".m4a": true, ".woff": true, ".woff2": true, ".pdf": true, ".dmg": true,
}

// worthCompressing decides from the name and the first bytes.
func worthCompressing(name string, head []byte) bool {
	if bytes.HasPrefix(head, zstdMagic) {
		return true
	}
	if compressedExt[strings.ToLower(filepath.Ext(name))] {
		return false
	}
	if len(head) < 512 {
		return true
	}
	// Sample: if the start barely shrinks, the rest will not either.
	small := encoder().EncodeAll(head, nil)
	return len(small) < len(head)*9/10
}

// zcopyFile copies src to dst, compressing it when that helps.
func zcopyFile(src, dst string, fi os.FileInfo) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if isSparse(src, fi) && !startsWithMagic(in) {
		// Keep a sparse file sparse as a plain copy with holes: compressing
		// would mean reading and expanding every hole.
		err = copySparse(in, out, fi.Size())
	} else {
		err = zcopy(in, out, src)
	}
	if err != nil {
		out.Close()
		os.Remove(dst)
		return err
	}
	return out.Close()
}

func startsWithMagic(f *os.File) bool {
	head := make([]byte, 4)
	n, _ := f.ReadAt(head, 0)
	return n == 4 && bytes.Equal(head, zstdMagic)
}

// zcopy streams r into w, compressed when worth it.
func zcopy(r io.Reader, w io.Writer, name string) error {
	br := bufio.NewReaderSize(r, 64<<10)
	head, _ := br.Peek(64 << 10)
	if !worthCompressing(name, head) {
		_, err := io.Copy(w, br)
		return err
	}
	zw, err := zstd.NewWriter(w, zstd.WithEncoderLevel(zstd.SpeedDefault), zstd.WithEncoderConcurrency(2))
	if err != nil {
		return err
	}
	if _, err := io.Copy(zw, br); err != nil {
		zw.Close()
		return err
	}
	return zw.Close()
}

// openStored opens a file from the store, decompressing it if needed.
func openStored(p string) (io.ReadCloser, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	br := bufio.NewReaderSize(f, 64<<10)
	head, _ := br.Peek(4)
	if !bytes.Equal(head, zstdMagic) {
		return struct {
			io.Reader
			io.Closer
		}{br, f}, nil
	}
	zr, err := zstd.NewReader(br, zstd.WithDecoderConcurrency(1))
	if err != nil {
		f.Close()
		return nil, err
	}
	return struct {
		io.Reader
		io.Closer
	}{zr, closeBoth{zr, f}}, nil
}

type closeBoth struct {
	z *zstd.Decoder
	f *os.File
}

func (c closeBoth) Close() error { c.z.Close(); return c.f.Close() }

// unzcopyFile writes the original bytes of stored file src to dst, keeping
// runs of zeros as holes.
func unzcopyFile(src, dst string, fi os.FileInfo) error {
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	fail := func(err error) error {
		out.Close()
		os.Remove(dst)
		return err
	}
	raw, err := os.Open(src)
	if err != nil {
		return fail(err)
	}
	defer raw.Close()
	if !startsWithMagic(raw) {
		// Stored as it was, holes included.
		if err := copySparse(raw, out, fi.Size()); err != nil {
			return fail(err)
		}
		return out.Close()
	}
	in, err := openStored(src)
	if err != nil {
		return fail(err)
	}
	defer in.Close()
	w := &holeWriter{f: out}
	if _, err := io.CopyBuffer(w, in, make([]byte, 1<<20)); err != nil {
		return fail(err)
	}
	if err := w.finish(); err != nil {
		return fail(err)
	}
	return out.Close()
}

// ReadStored returns the original bytes of a stored file, for diffs.
func ReadStored(p string) ([]byte, error) {
	r, err := openStored(p)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	return io.ReadAll(r)
}
