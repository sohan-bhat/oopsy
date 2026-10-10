package snap

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
)

// Record is saved state that is not a file tree or a git ref: Kubernetes
// objects, Terraform state, a Docker volume, the version of a file an agent
// was about to edit. Each kind registers how it is restored.
type Record struct {
	Kind  string            `json:"kind"`
	Label string            `json:"label"`         // what undo brings back, in one line
	Dir   string            `json:"dir,omitempty"` // folder inside the snapshot holding its data
	Meta  map[string]string `json:"meta,omitempty"`
	Size  int64             `json:"size"`
}

// RecordDir is where r's data lives.
func (s *Snapshot) RecordDir(r Record) string { return filepath.Join(s.Dir(), r.Dir) }

// Capture saves something into dir (which exists and is empty) and
// describes it. Returning a nil record means there was nothing to save.
type Capture func(dir string) (*Record, error)

// RecordKind restores one kind of record.
type RecordKind struct {
	// Restore puts the saved state back.
	Restore func(s *Snapshot, r Record) Outcome
	// Objects lists object store hashes the record uses, so pruning keeps them.
	Objects func(r Record) []string
}

var (
	kindsMu sync.Mutex
	kinds   = map[string]RecordKind{}
)

// RegisterRecordKind makes a record kind restorable.
func RegisterRecordKind(kind string, k RecordKind) {
	kindsMu.Lock()
	defer kindsMu.Unlock()
	kinds[kind] = k
}

func recordKind(kind string) (RecordKind, bool) {
	kindsMu.Lock()
	defer kindsMu.Unlock()
	k, ok := kinds[kind]
	return k, ok
}

// runCaptures saves every capture into its own folder of s.
func (s *Snapshot) runCaptures(caps []Capture) {
	for i, c := range caps {
		rel := filepath.Join("records", fmt.Sprint(len(s.Records)+i))
		dir := filepath.Join(s.Dir(), rel)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			s.Skipped = append(s.Skipped, Skip{Path: dir, Reason: skipReason(err)})
			continue
		}
		r, err := c(dir)
		if err != nil {
			_ = forceRemove(dir)
			s.Skipped = append(s.Skipped, Skip{Path: "", Reason: err.Error()})
			continue
		}
		if r == nil {
			_ = forceRemove(dir)
			continue
		}
		r.Dir = rel
		if r.Size == 0 {
			r.Size, _, _ = treeSize(dir)
		}
		s.Records = append(s.Records, *r)
		s.Size += r.Size
	}
	_ = os.Remove(filepath.Join(s.Dir(), "records")) // only when empty
}

func restoreRecord(s *Snapshot, r Record) Outcome {
	k, ok := recordKind(r.Kind)
	if !ok || k.Restore == nil {
		return Outcome{Path: r.Label, Status: "failed", Detail: "this version of oops cannot restore " + r.Kind + " records"}
	}
	return k.Restore(s, r)
}

// recordObjects lists object hashes used by records in s.
func (s *Snapshot) recordObjects() []string {
	var out []string
	for _, r := range s.Records {
		if k, ok := recordKind(r.Kind); ok && k.Objects != nil {
			out = append(out, k.Objects(r)...)
		}
	}
	sort.Strings(out)
	return out
}
