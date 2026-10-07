package snap

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// The object store keeps file versions by content hash, zstd compressed, so
// identical versions are stored once however many snapshots use them. Agent
// edits and watch mode save single files here.

func objectsDir() string { return filepath.Join(Dir(), "objects") }

// ObjectPath is where the object with this sha256 hash lives.
func ObjectPath(hash string) string {
	if len(hash) < 3 {
		return filepath.Join(objectsDir(), hash)
	}
	return filepath.Join(objectsDir(), hash[:2], hash[2:])
}

// Object describes a stored file version.
type Object struct {
	Hash   string
	Size   int64 // original size
	Stored int64 // bytes on disk, zero when it was already stored
}

// PutObject stores the regular file at src and returns its hash. Storing
// the same content again costs nothing.
func PutObject(src string) (Object, error) {
	f, err := os.Open(src)
	if err != nil {
		return Object{}, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return Object{}, err
	}
	if !fi.Mode().IsRegular() {
		return Object{}, errors.New("not a regular file")
	}
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return Object{}, err
	}
	o := Object{Hash: hex.EncodeToString(h.Sum(nil)), Size: fi.Size()}
	dst := ObjectPath(o.Hash)
	if _, err := os.Stat(dst); err == nil {
		now := time.Now()
		_ = os.Chtimes(dst, now, now) // keep it from looking unused while pruning
		return o, nil
	}
	if sp, err := SpaceAt(Dir(), LoadConfig()); err == nil && !sp.Allows(fi.Size()) {
		return Object{}, errNoSpace(fi.Size(), sp)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return Object{}, err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return Object{}, err
	}
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".tmp-*")
	if err != nil {
		return Object{}, err
	}
	if err := zcopy(f, tmp, src); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return Object{}, err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return Object{}, err
	}
	if err := os.Rename(tmp.Name(), dst); err != nil {
		os.Remove(tmp.Name())
		return Object{}, err
	}
	if st, err := os.Stat(dst); err == nil {
		o.Stored = st.Size()
	}
	return o, nil
}

// ReadObject returns the original bytes of an object.
func ReadObject(hash string) ([]byte, error) { return ReadStored(ObjectPath(hash)) }

// pruneObjects deletes objects no snapshot uses any more. Objects written in
// the last hour are kept, since a snapshot may be about to reference them.
func pruneObjects(used map[string]bool) (int, int64) {
	var n int
	var freed int64
	cutoff := time.Now().Add(-time.Hour)
	_ = filepath.WalkDir(objectsDir(), func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		hash := filepath.Base(filepath.Dir(p)) + d.Name()
		if used[hash] {
			return nil
		}
		fi, err := d.Info()
		if err != nil || fi.ModTime().After(cutoff) {
			return nil
		}
		if os.Remove(p) == nil {
			n++
			freed += fi.Size()
		}
		return nil
	})
	return n, freed
}

// objectsUsed lists every object any snapshot references.
func objectsUsed(list []*Snapshot) map[string]bool {
	used := map[string]bool{}
	for _, s := range list {
		for _, e := range s.Entries {
			if e.Method == "object" {
				used[e.Store] = true
			}
		}
		for _, h := range s.recordObjects() {
			used[h] = true
		}
	}
	return used
}

// SaveVersions snapshots single files into the object store before a tool
// rewrites them, such as an agent's edit tool. Missing paths are skipped:
// the edit creates them, and there is nothing to lose.
func SaveVersions(command, cwd, source string, paths []string) (*Snapshot, error) {
	start := time.Now()
	l, err := acquire()
	if err != nil {
		return nil, err
	}
	defer l.release()
	var entries []Entry
	var skipped []Skip
	for _, p := range minimalPaths(paths) {
		fi, err := os.Lstat(p)
		if err != nil || !fi.Mode().IsRegular() {
			continue
		}
		if reason := refuse(p); reason != "" {
			skipped = append(skipped, Skip{Path: p, Reason: reason})
			continue
		}
		o, err := PutObject(p)
		if err != nil {
			skipped = append(skipped, Skip{Path: p, Reason: skipReason(err)})
			continue
		}
		entries = append(entries, Entry{Orig: p, Store: o.Hash, Type: "file", Method: "object", Size: o.Size, Files: 1,
			Disk: o.Stored, Mode: uint32(fi.Mode().Perm()), Mtime: fi.ModTime().UnixNano()})
	}
	if len(entries) == 0 && len(skipped) == 0 {
		return nil, nil
	}
	id, err := nextID()
	if err != nil {
		return nil, err
	}
	s := &Snapshot{ID: id, Time: start, Command: command, Cwd: cwd, Source: source, Entries: entries, Skipped: skipped}
	for _, e := range entries {
		s.Size += e.Size
		s.Disk += e.Disk
		s.Files++
	}
	if err := os.MkdirAll(s.Dir(), 0o700); err != nil {
		return nil, err
	}
	s.Millis = float64(time.Since(start).Microseconds()) / 1000
	if err := s.save(); err != nil {
		return nil, err
	}
	appendJournal(s)
	return s, nil
}

// SetAfter records what a command left at each saved path, so undo can
// swap the file back while nobody else has touched it.
func SetAfter(id int) error {
	s, err := Load(id)
	if err != nil {
		return err
	}
	changed := false
	for i, e := range s.Entries {
		if e.Type == "file" && e.After == nil {
			if st := StampOf(e.Orig); st != nil {
				s.Entries[i].After = st
				changed = true
			}
		}
	}
	if !changed {
		return nil
	}
	return s.save()
}
