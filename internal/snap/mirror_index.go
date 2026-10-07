package snap

import (
	"bufio"
	"encoding/binary"
	"errors"
	"io"
	"os"
)

// The mirror index is a small binary file: a magic header, the entry count,
// then each entry as varints. It loads in about a millisecond for 10,000
// files, which matters because hooks read it before every command.

var indexMagic = []byte("OOPSMIR1")

func writeIndex(p string, nodes []node) error {
	tmp := p + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	w := bufio.NewWriterSize(f, 256<<10)
	w.Write(indexMagic)
	var buf [binary.MaxVarintLen64]byte
	put := func(v uint64) { w.Write(buf[:binary.PutUvarint(buf[:], v)]) }
	puts := func(v int64) { w.Write(buf[:binary.PutVarint(buf[:], v)]) }
	put(uint64(len(nodes)))
	for _, n := range nodes {
		put(uint64(len(n.rel)))
		w.WriteString(n.rel)
		w.WriteByte(n.kind)
		puts(n.size)
		puts(n.mtime)
		put(uint64(n.mode))
	}
	if err := w.Flush(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

func readIndex(p string) ([]node, error) {
	b, err := os.ReadFile(p)
	if err != nil {
		return nil, err
	}
	if len(b) < len(indexMagic) || string(b[:len(indexMagic)]) != string(indexMagic) {
		return nil, errors.New("not a mirror index")
	}
	b = b[len(indexMagic):]
	bad := io.ErrUnexpectedEOF
	get := func() (uint64, error) {
		v, n := binary.Uvarint(b)
		if n <= 0 {
			return 0, bad
		}
		b = b[n:]
		return v, nil
	}
	gets := func() (int64, error) {
		v, n := binary.Varint(b)
		if n <= 0 {
			return 0, bad
		}
		b = b[n:]
		return v, nil
	}
	count, err := get()
	if err != nil || count > 1<<24 {
		return nil, bad
	}
	nodes := make([]node, 0, count)
	for i := uint64(0); i < count; i++ {
		l, err := get()
		if err != nil || uint64(len(b)) < l+1 {
			return nil, bad
		}
		n := node{rel: string(b[:l]), kind: b[l]}
		b = b[l+1:]
		if n.size, err = gets(); err != nil {
			return nil, err
		}
		if n.mtime, err = gets(); err != nil {
			return nil, err
		}
		m, err := get()
		if err != nil {
			return nil, err
		}
		n.mode = uint32(m)
		nodes = append(nodes, n)
	}
	return nodes, nil
}
