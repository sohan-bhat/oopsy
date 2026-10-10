// Package snap stores and restores file and git snapshots under ~/.oops.
package snap

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Config holds user settings from ~/.oops/config.json.
type Config struct {
	KeepDays     int   `json:"keep_days"`
	MaxBytes     int64 `json:"max_bytes"`
	CopyCapBytes int64 `json:"copy_cap_bytes"`
	Quiet        bool  `json:"quiet"`
}

// DefaultConfig is used when no config file exists.
var DefaultConfig = Config{KeepDays: 7, MaxBytes: 5 << 30, CopyCapBytes: 1 << 30}

// Dir returns the Oopsy home directory ($OOPS_HOME or ~/.oops).
func Dir() string {
	if d := os.Getenv("OOPS_HOME"); d != "" {
		return d
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(os.TempDir(), "oops")
	}
	return filepath.Join(home, ".oops")
}

func snapsDir() string { return filepath.Join(Dir(), "snapshots") }

// SnapDir returns the directory for snapshot id.
func SnapDir(id int) string { return filepath.Join(snapsDir(), strconv.Itoa(id)) }

// ConfigPath is where settings live.
func ConfigPath() string { return filepath.Join(Dir(), "config.json") }

// LoadConfig reads settings, filling in defaults.
func LoadConfig() Config {
	cfg := DefaultConfig
	b, err := os.ReadFile(ConfigPath())
	if err == nil {
		_ = json.Unmarshal(b, &cfg)
	}
	if cfg.KeepDays <= 0 {
		cfg.KeepDays = DefaultConfig.KeepDays
	}
	if cfg.MaxBytes <= 0 {
		cfg.MaxBytes = DefaultConfig.MaxBytes
	}
	if cfg.CopyCapBytes <= 0 {
		cfg.CopyCapBytes = DefaultConfig.CopyCapBytes
	}
	return cfg
}

// SaveConfig writes settings.
func SaveConfig(cfg Config) error {
	if err := os.MkdirAll(Dir(), 0o700); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(cfg, "", "  ")
	return os.WriteFile(ConfigPath(), append(b, '\n'), 0o600)
}

// Entry is one saved path.
type Entry struct {
	Orig        string `json:"orig"`
	Store       string `json:"store"`
	Type        string `json:"type"` // file, dir, symlink
	Size        int64  `json:"size"`
	Files       int    `json:"files"`
	Method      string `json:"method"` // clone, reflink, block clone, hardlink, copy, compressed copy
	ParentMtime int64  `json:"parent_mtime,omitempty"`
	GitRepo     string `json:"git_repo,omitempty"` // set when git would have overwritten a tracked file
	// Disk is the extra disk the snapshot took when it was made: zero for
	// clones and hard links, which share the original's data.
	Disk int64 `json:"disk"`
	// Attrs holds what a delete may clear on hard linked files, keyed by path
	// relative to Orig: Windows attributes (read only, hidden, system), or
	// the mode of read only files on Unix.
	Attrs map[string]uint32 `json:"attrs,omitempty"`
	// Base is set when the data lives in a store on another volume.
	Base string `json:"base,omitempty"`
}

// EntryPath is where entry e's saved data lives.
func (s *Snapshot) EntryPath(e Entry) string {
	if e.Base != "" {
		return filepath.Join(e.Base, e.Store)
	}
	return filepath.Join(s.Dir(), e.Store)
}

// GitRecord is git state saved before a git command.
type GitRecord struct {
	Kind    string `json:"kind"`
	Repo    string `json:"repo"`
	Head    string `json:"head,omitempty"`
	Ref     string `json:"ref,omitempty"`
	SHA     string `json:"sha,omitempty"`
	Remote  string `json:"remote,omitempty"`
	Message string `json:"message,omitempty"`
}

// Skip records a path that could not be saved.
type Skip struct {
	Path   string `json:"path"`
	Reason string `json:"reason"`
}

// Snapshot is the metadata for one saved destructive command.
type Snapshot struct {
	ID       int         `json:"id"`
	Time     time.Time   `json:"time"`
	Command  string      `json:"command"`
	Cwd      string      `json:"cwd"`
	Source   string      `json:"source"`
	Programs []string    `json:"programs"`
	Entries  []Entry     `json:"entries,omitempty"`
	Git      []GitRecord `json:"git,omitempty"`
	Skipped  []Skip      `json:"skipped,omitempty"`
	Notes    []string    `json:"notes,omitempty"`
	Size     int64       `json:"size"`
	Disk     int64       `json:"disk"`
	Files    int         `json:"files"`
	Broad    bool        `json:"broad,omitempty"`
	Millis   float64     `json:"millis"`
	Restored *time.Time  `json:"restored,omitempty"`

	vols          map[string]string // volume root -> this snapshot's dir in its volume store
	compressTried bool
}

// Dir returns the snapshot's directory.
func (s *Snapshot) Dir() string { return SnapDir(s.ID) }

func (s *Snapshot) save() error {
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(s.Dir(), "meta.json.tmp")
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(s.Dir(), "meta.json"))
}

// Load reads snapshot id.
func Load(id int) (*Snapshot, error) {
	b, err := os.ReadFile(filepath.Join(SnapDir(id), "meta.json"))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("no snapshot %d", id)
		}
		return nil, err
	}
	var s Snapshot
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

// List returns all snapshots, newest first.
func List() ([]*Snapshot, error) {
	ents, err := os.ReadDir(snapsDir())
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var out []*Snapshot
	for _, e := range ents {
		id, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		s, err := Load(id)
		if err != nil {
			continue
		}
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID > out[j].ID })
	return out, nil
}

// Latest returns the newest snapshot that has not been restored yet.
func Latest() (*Snapshot, error) {
	all, err := List()
	if err != nil {
		return nil, err
	}
	for _, s := range all {
		if s.Restored == nil {
			return s, nil
		}
	}
	return nil, nil
}

func nextID() (int, error) {
	p := filepath.Join(Dir(), "next-id")
	n := 1
	if b, err := os.ReadFile(p); err == nil {
		if v, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil && v > 0 {
			n = v
		}
	}
	for {
		if _, err := os.Lstat(SnapDir(n)); errors.Is(err, fs.ErrNotExist) {
			break
		}
		n++
	}
	if err := os.WriteFile(p, []byte(strconv.Itoa(n+1)), 0o600); err != nil {
		return 0, err
	}
	return n, nil
}

func appendJournal(s *Snapshot) {
	f, err := os.OpenFile(filepath.Join(Dir(), "journal.jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	var paths []string
	for _, e := range s.Entries {
		paths = append(paths, e.Orig)
	}
	rec := map[string]any{
		"id": s.ID, "time": s.Time, "source": s.Source, "cwd": s.Cwd, "command": s.Command,
		"paths": paths, "bytes": s.Size, "files": s.Files, "git": len(s.Git),
	}
	b, _ := json.Marshal(rec)
	w := bufio.NewWriter(f)
	w.Write(b)
	w.WriteByte('\n')
	w.Flush()
}

// Remove deletes a snapshot, including read only directories inside it and
// its data in volume stores.
func Remove(id int) error {
	if s, err := Load(id); err == nil {
		for _, e := range s.Entries {
			if e.Base != "" && filepath.Base(e.Base) == strconv.Itoa(id) {
				_ = forceRemove(e.Base)
			}
		}
	}
	return forceRemove(SnapDir(id))
}

func forceRemove(p string) error {
	if err := os.RemoveAll(p); err == nil {
		return nil
	}
	_ = filepath.WalkDir(p, func(path string, d fs.DirEntry, err error) error {
		if err == nil && d.IsDir() {
			_ = os.Chmod(path, 0o700)
		}
		return nil
	})
	return os.RemoveAll(p)
}

// PruneResult reports what Prune removed.
type PruneResult struct {
	Removed int
	Bytes   int64
}

// Prune drops snapshots older than the configured age, then the oldest ones
// until the total size fits under the cap.
func Prune(cfg Config, all bool) (PruneResult, error) {
	var res PruneResult
	list, err := List()
	if err != nil {
		return res, err
	}
	sort.Slice(list, func(i, j int) bool { return list[i].ID < list[j].ID })
	cutoff := time.Now().Add(-time.Duration(cfg.KeepDays) * 24 * time.Hour)
	var total int64
	var keep []*Snapshot
	for _, s := range list {
		if all || s.Time.Before(cutoff) {
			if Remove(s.ID) == nil {
				res.Removed++
				res.Bytes += s.Size
			}
			continue
		}
		keep = append(keep, s)
		total += s.Size
	}
	for len(keep) > 1 && total > cfg.MaxBytes {
		s := keep[0]
		keep = keep[1:]
		if Remove(s.ID) == nil {
			res.Removed++
			res.Bytes += s.Size
			total -= s.Size
		}
	}
	return res, nil
}

// Usage returns the number of snapshots and their total size.
func Usage() (int, int64) {
	list, _ := List()
	var total int64
	for _, s := range list {
		total += s.Size
	}
	return len(list), total
}
