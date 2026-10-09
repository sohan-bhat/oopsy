// Package watch is oops watch: an optional background process that keeps
// project mirrors current between commands, so editor saves, agent edit
// tools and deletions made outside a hooked shell can be undone too.
package watch

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"github.com/sohan-bhat/oopsy/internal/snap"
)

// Status is what the daemon reports about itself. It writes it to a file
// every few seconds, which oops watch status reads.
type Status struct {
	PID       int       `json:"pid"`
	Started   time.Time `json:"started"`
	Updated   time.Time `json:"updated"`
	Mechanism string    `json:"mechanism"` // inotify, FSEvents, ReadDirectoryChangesW, polling
	Projects  []string  `json:"projects"`
	Polled    []string  `json:"polled,omitempty"` // projects checked by a periodic stat walk instead
	Ready     bool      `json:"ready"`            // every project has had its first sync
	Events    uint64    `json:"events"`
	Syncs     uint64    `json:"syncs"`
	Snapshots uint64    `json:"snapshots"`
	// LastSnapshot is the newest watch snapshot and LatencyMs the time from
	// the first event of its change to the snapshot being saved.
	LastSnapshot int      `json:"last_snapshot,omitempty"`
	LatencyMs    float64  `json:"latency_ms,omitempty"`
	RSS          uint64   `json:"rss"`  // resident memory, bytes
	Heap         uint64   `json:"heap"` // Go heap in use, bytes
	BaseFiles    int      `json:"base_files,omitempty"`
	BaseBytes    int64    `json:"base_bytes,omitempty"`
	Notes        []string `json:"notes,omitempty"`
	LastError    string   `json:"last_error,omitempty"`
}

// Dir is where watch mode keeps its state.
func Dir() string { return filepath.Join(snap.Dir(), "watch") }

func statusPath() string  { return filepath.Join(Dir(), "status.json") }
func pidPath() string     { return filepath.Join(Dir(), "pid") }
func servicePath() string { return filepath.Join(Dir(), "service.json") }
func logPath() string     { return filepath.Join(Dir(), "log") }

// ReadStatus returns the daemon's last report, if any.
func ReadStatus() (*Status, error) {
	b, err := os.ReadFile(statusPath())
	if err != nil {
		return nil, err
	}
	var st Status
	if err := json.Unmarshal(b, &st); err != nil {
		return nil, err
	}
	return &st, nil
}

func writeStatus(st *Status) {
	b, _ := json.MarshalIndent(st, "", "  ")
	tmp := statusPath() + ".tmp"
	if os.WriteFile(tmp, b, 0o600) == nil {
		_ = os.Rename(tmp, statusPath())
	}
}

// Running reports the daemon's status when its process is alive.
func Running() (*Status, bool) {
	st, err := ReadStatus()
	if err != nil || st.PID <= 0 {
		return st, false
	}
	return st, alive(st.PID) && time.Since(st.Updated) < 30*time.Second
}

// serviceInfo records how oops watch on started the daemon.
type serviceInfo struct {
	Manager string    `json:"manager"` // launchd, systemd, task or pid
	Path    string    `json:"path"`    // the plist, unit or task name
	Exe     string    `json:"exe"`
	Since   time.Time `json:"since"`
}

func readService() *serviceInfo {
	b, err := os.ReadFile(servicePath())
	if err != nil {
		return nil
	}
	var si serviceInfo
	if json.Unmarshal(b, &si) != nil {
		return nil
	}
	return &si
}

func writeService(si serviceInfo) error {
	if err := os.MkdirAll(Dir(), 0o700); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(si, "", "  ")
	return os.WriteFile(servicePath(), b, 0o600)
}
