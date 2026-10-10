package snap

import (
	"bufio"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"time"
)

// On disks where the mirror uses hard links (NTFS, ext4), a program that
// writes into a file in place changes the mirror's link too, so the mirror
// alone cannot keep the previous version. While watch mode is on, every
// mirrored file under the size limit also has a compressed copy in the
// object store, deduplicated by content. The watch base of a mirror records
// which object holds which version. When a file changes in place, its base
// object becomes the pre write copy the sync attributes.

var baseMagic = []byte("OOPSWB01")

// baseRec is the stored version of one file.
type baseRec struct {
	size  int64
	mtime int64
	mode  uint32
	hash  [32]byte
}

func (b baseRec) stamp() Stamp { return Stamp{Size: b.size, Mtime: b.mtime} }

func (b baseRec) hex() string { return hex.EncodeToString(b.hash[:]) }

func (m *Mirror) basePath() string { return filepath.Join(m.Dir, "watch-base") }

// hardlinked reports a mirror that cannot keep in place writes by itself.
func (m *Mirror) hardlinked() bool { return m.Method == "hardlink" }

func readBase(p string) (map[string]baseRec, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	r := bufio.NewReaderSize(f, 64<<10)
	head := make([]byte, len(baseMagic))
	if _, err := io.ReadFull(r, head); err != nil || string(head) != string(baseMagic) {
		return nil, errors.New("not a watch base")
	}
	count, err := binary.ReadUvarint(r)
	if err != nil || count > 1<<24 {
		return nil, errors.New("bad watch base")
	}
	out := make(map[string]baseRec, count)
	for i := uint64(0); i < count; i++ {
		l, err := binary.ReadUvarint(r)
		if err != nil || l > 1<<16 {
			return nil, errors.New("bad watch base")
		}
		name := make([]byte, l)
		if _, err := io.ReadFull(r, name); err != nil {
			return nil, err
		}
		var b baseRec
		if b.size, err = binary.ReadVarint(r); err != nil {
			return nil, err
		}
		if b.mtime, err = binary.ReadVarint(r); err != nil {
			return nil, err
		}
		mode, err := binary.ReadUvarint(r)
		if err != nil {
			return nil, err
		}
		b.mode = uint32(mode)
		if _, err := io.ReadFull(r, b.hash[:]); err != nil {
			return nil, err
		}
		out[string(name)] = b
	}
	return out, nil
}

func writeBase(p string, recs map[string]baseRec) error {
	tmp := p + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	w := bufio.NewWriterSize(f, 64<<10)
	w.Write(baseMagic)
	var buf [binary.MaxVarintLen64]byte
	put := func(v uint64) { w.Write(buf[:binary.PutUvarint(buf[:], v)]) }
	puts := func(v int64) { w.Write(buf[:binary.PutVarint(buf[:], v)]) }
	put(uint64(len(recs)))
	for name, b := range recs {
		put(uint64(len(name)))
		w.WriteString(name)
		puts(b.size)
		puts(b.mtime)
		put(uint64(b.mode))
		w.Write(b.hash[:])
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

// BaseResult reports what UpdateWatchBase did.
type BaseResult struct {
	Files  int   // files with a stored version
	Stored int   // objects written or refreshed in this pass
	Bytes  int64 // compressed bytes all base objects of this mirror take
	Raw    int64 // original bytes of those files
}

// UpdateWatchBase stores a compressed copy of every mirrored file of root
// up to maxFile bytes whose current version is not stored yet. It only
// matters for hard link mirrors and does nothing on clone mirrors. It runs
// without the mirror lock, since it only reads the project and the index.
func UpdateWatchBase(root string, maxFile int64, verify bool) (BaseResult, error) {
	var res BaseResult
	m, err := OpenMirror(root)
	if err != nil {
		return res, err
	}
	if !m.hardlinked() || !m.Exists() {
		return res, nil
	}
	idx, err := readIndex(m.indexPath())
	if err != nil {
		return res, err
	}
	old, _ := readBase(m.basePath())
	recs := make(map[string]baseRec, len(idx))
	changed := len(old) == 0
	for _, n := range idx {
		if n.kind != 'f' || n.size > maxFile {
			continue
		}
		if b, ok := old[n.rel]; ok && b.stamp() == n.stamp() {
			if !verify || exists(ObjectPath(b.hex())) {
				recs[n.rel] = b
				res.Raw += b.size
				continue
			}
		}
		changed = true
		orig := m.origPath(n.rel)
		fi, err := os.Lstat(orig)
		if err != nil || !fi.Mode().IsRegular() || fi.Size() != n.size || fi.ModTime().UnixNano() != n.mtime {
			continue // changed since the last sync; the next one catches up
		}
		o, err := PutObject(orig)
		if err != nil {
			continue
		}
		if st := StampOf(orig); st == nil || *st != n.stamp() {
			continue // written while it was being read
		}
		b := baseRec{size: n.size, mtime: n.mtime, mode: uint32(fi.Mode().Perm())}
		if raw, err := hex.DecodeString(o.Hash); err == nil && len(raw) == 32 {
			copy(b.hash[:], raw)
		} else {
			continue
		}
		recs[n.rel] = b
		res.Stored++
		res.Raw += b.size
	}
	if len(recs) != len(old) {
		changed = true
	}
	res.Files = len(recs)
	if changed {
		if err := writeBase(m.basePath(), recs); err != nil {
			return res, err
		}
	}
	return res, nil
}

// BaseCost adds up the disk the base objects of root take.
func BaseCost(root string) BaseResult {
	var res BaseResult
	m, err := OpenMirror(root)
	if err != nil {
		return res
	}
	recs, _ := readBase(m.basePath())
	seen := map[[32]byte]bool{}
	for _, b := range recs {
		res.Files++
		res.Raw += b.size
		if seen[b.hash] {
			continue
		}
		seen[b.hash] = true
		if fi, err := os.Stat(ObjectPath(b.hex())); err == nil {
			res.Bytes += fi.Size()
		}
	}
	return res
}

// prepPrewrite puts the stored previous version of every file of a hard
// link mirror that was written in place into the pre write folder, where
// the sync picks it up (and clears what it does not use). It returns the
// pre write files it made, by the original path, with the object each came
// from.
func (m *Mirror) prepPrewrite(old []node) map[string]baseRec {
	if !m.hardlinked() {
		return nil
	}
	recs, err := readBase(m.basePath())
	if err != nil || len(recs) == 0 {
		return nil
	}
	made := map[string]baseRec{}
	for _, n := range old {
		if n.kind != 'f' {
			continue
		}
		b, ok := recs[n.rel]
		if !ok || b.stamp() != n.stamp() {
			continue
		}
		orig := m.origPath(n.rel)
		cur := StampOf(orig)
		if cur == nil || *cur == n.stamp() {
			continue // unchanged, or gone (the mirror link still has it)
		}
		if ds := StampOf(m.dataPath(n.rel)); ds != nil && *ds == n.stamp() {
			continue // replaced by a new file: the mirror's link kept the old one
		}
		dst := filepath.Join(m.prewriteDir(), PrewriteName(orig))
		if st := StampOf(dst); st != nil && *st == n.stamp() {
			continue // a pre write hook already saved it
		}
		if writePrewrite(ObjectPath(b.hex()), dst, n) == nil {
			made[orig] = b
		}
	}
	return made
}

func writePrewrite(obj, dst string, n node) error {
	in, err := openStored(obj)
	if err != nil {
		return err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	tmp := dst + ".tmp"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	written, err := io.Copy(out, in)
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err == nil && written != n.size {
		err = errors.New("stored version has the wrong size")
	}
	if err == nil {
		t := time.Unix(0, n.mtime)
		err = os.Chtimes(tmp, t, t)
	}
	if err == nil {
		err = os.Rename(tmp, dst)
	}
	if err != nil {
		os.Remove(tmp)
	}
	return err
}

// WatchBaseObjects lists the objects the watch bases of all mirrors use, so
// pruning keeps them.
func WatchBaseObjects() map[string]bool {
	used := map[string]bool{}
	for _, r := range WatchRoots() {
		m, err := OpenMirror(r)
		if err != nil {
			continue
		}
		recs, _ := readBase(m.basePath())
		for _, b := range recs {
			used[b.hex()] = true
		}
	}
	return used
}

// DropWatchBases deletes the watch base of every mirror, when watch mode is
// turned off. The objects they used are pruned with the next prune.
func DropWatchBases() {
	for _, r := range WatchRoots() {
		if m, err := OpenMirror(r); err == nil {
			_ = os.Remove(m.basePath())
		}
	}
}
