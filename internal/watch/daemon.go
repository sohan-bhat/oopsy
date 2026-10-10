package watch

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"runtime/debug"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/sohan-bhat/oopsy/internal/snap"
)

// A backend turns file system events into calls to notify. Set replaces
// the watched projects and returns the ones it cannot watch, which the
// daemon then checks with a periodic stat walk instead.
type backend interface {
	Name() string
	Set(roots []string) (poll []string)
	Close()
}

// notifyFunc is called for every change: path is the changed path, or ""
// with full set when events were lost and the whole project must be
// compared.
type notifyFunc func(root, path string, full bool)

const (
	debounce   = 300 * time.Millisecond
	maxDelay   = 3 * time.Second // a steady stream of events still syncs this often
	pollEvery  = 30 * time.Second
	statusTick = 2 * time.Second
	keepWatch  = 2000 // watch snapshots of edits kept at most
)

type rootState struct {
	dirty       bool
	first, last time.Time
	polled      bool
	nextPoll    time.Time
	started     bool
}

type daemon struct {
	mu    sync.Mutex
	roots map[string]*rootState
	st    Status
	be    backend
	notes map[string]bool
}

// Options tune the daemon, for tests.
type Options struct {
	Rescan time.Duration // how often new mirrors are picked up
}

// Run is the daemon itself, in the foreground, until ctx ends.
func Run(ctx context.Context, opt Options) error {
	if opt.Rescan <= 0 {
		opt.Rescan = 15 * time.Second
		if v, err := time.ParseDuration(os.Getenv("OOPS_WATCH_RESCAN")); err == nil && v > 0 {
			opt.Rescan = v
		}
	}
	if err := os.MkdirAll(Dir(), 0o700); err != nil {
		return err
	}
	unlock, err := singleInstance()
	if err != nil {
		return err
	}
	defer unlock()
	hideConsole()
	// Keep the footprint small: the daemon runs all day.
	debug.SetGCPercent(25)
	debug.SetMemoryLimit(40 << 20)

	d := &daemon{roots: map[string]*rootState{}, notes: map[string]bool{}}
	d.st = Status{PID: os.Getpid(), Started: time.Now()}
	_ = os.WriteFile(pidPath(), []byte(strconv.Itoa(os.Getpid())), 0o600)
	defer os.Remove(pidPath())
	d.be, err = newBackend(d.notify)
	if err != nil {
		d.note(fmt.Sprintf("no file system events (%v); checking every %s instead", err, pollEvery))
		d.be = pollBackend{}
	}
	defer d.be.Close()
	d.st.Mechanism = d.be.Name()

	d.rescan()
	rescan := time.NewTicker(opt.Rescan)
	defer rescan.Stop()
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	status := time.NewTicker(statusTick)
	defer status.Stop()
	prune := time.NewTicker(time.Hour)
	defer prune.Stop()
	d.writeStatus()
	for {
		select {
		case <-ctx.Done():
			d.writeStatus()
			return nil
		case <-rescan.C:
			d.rescan()
		case <-prune.C:
			snap.PruneWatch(keepWatch)
			_, _ = snap.Prune(snap.LoadConfig(), false)
		case <-status.C:
			d.writeStatus()
		case <-tick.C:
		}
		d.work(ctx)
	}
}

// notify records a change; the work loop syncs once events settle.
func (d *daemon) notify(root, path string, full bool) {
	if !full && snap.WatchIgnored(root, path) {
		return
	}
	now := time.Now()
	d.mu.Lock()
	d.st.Events++
	rs := d.roots[root]
	if rs != nil {
		if !rs.dirty {
			rs.dirty, rs.first = true, now
		}
		rs.last = now
	}
	d.mu.Unlock()
}

// rescan picks up mirrors made since the last look and drops removed ones.
func (d *daemon) rescan() {
	roots := snap.WatchRoots()
	sort.Strings(roots)
	d.mu.Lock()
	changed := len(roots) != len(d.roots)
	keep := map[string]bool{}
	for _, r := range roots {
		keep[r] = true
		if d.roots[r] == nil {
			changed = true
		}
	}
	d.mu.Unlock()
	if !changed {
		return
	}
	poll := d.be.Set(roots)
	polled := map[string]bool{}
	for _, r := range poll {
		polled[r] = true
	}
	now := time.Now()
	d.mu.Lock()
	for r := range d.roots {
		if !keep[r] {
			delete(d.roots, r)
		}
	}
	for _, r := range roots {
		rs := d.roots[r]
		if rs == nil {
			rs = &rootState{}
			d.roots[r] = rs
		}
		rs.polled = polled[r]
		if rs.polled && rs.nextPoll.IsZero() {
			rs.nextPoll = now.Add(pollEvery)
		}
	}
	d.st.Projects = roots
	d.st.Polled = poll
	d.mu.Unlock()
}

// work syncs every project whose events have settled. Syncs run one at a
// time, as the mirror code expects.
func (d *daemon) work(ctx context.Context) {
	for ctx.Err() == nil {
		root, first, initial := d.next()
		if root == "" {
			return
		}
		max := snap.WatchMaxFile()
		res, err := snap.WatchSync(root, max)
		var base snap.BaseResult
		if err == nil {
			base, err = snap.UpdateWatchBase(root, max, initial)
		}
		d.mu.Lock()
		d.st.Syncs++
		if err != nil {
			d.st.LastError = fmt.Sprintf("%s: %v", root, err)
		}
		if res != nil && res.Snapshot != nil {
			d.st.Snapshots++
			d.st.LastSnapshot = res.Snapshot.ID
			d.st.LatencyMs = float64(time.Since(first).Microseconds()) / 1000
		}
		if initial && base.Files > 0 {
			cost := snap.BaseCost(root)
			d.st.BaseFiles += cost.Files
			d.st.BaseBytes += cost.Bytes
		}
		if rs := d.roots[root]; rs != nil {
			rs.started = true
		}
		readyNow := d.allStarted() && !d.st.Ready
		d.st.Ready = d.allStarted()
		d.mu.Unlock()
		if res != nil && res.Snapshot != nil && res.Snapshot.ID%100 == 0 {
			snap.PruneWatch(keepWatch)
		}
		if initial || readyNow {
			debug.FreeOSMemory()
			d.writeStatus()
		}
	}
}

// next picks a project that is due for a sync.
func (d *daemon) next() (root string, first time.Time, initial bool) {
	now := time.Now()
	d.mu.Lock()
	defer d.mu.Unlock()
	var keys []string
	for r := range d.roots {
		keys = append(keys, r)
	}
	sort.Strings(keys)
	for _, r := range keys {
		rs := d.roots[r]
		switch {
		case !rs.started:
			rs.dirty = false
			return r, now, true
		case rs.dirty && (now.Sub(rs.last) >= debounce || now.Sub(rs.first) >= maxDelay):
			rs.dirty = false
			return r, rs.first, false
		case rs.polled && now.After(rs.nextPoll):
			rs.nextPoll = now.Add(pollEvery)
			return r, now, false
		}
	}
	return "", now, false
}

// allStarted reports whether every project has had its first sync. The
// caller holds mu.
func (d *daemon) allStarted() bool {
	for _, rs := range d.roots {
		if !rs.started {
			return false
		}
	}
	return true
}

func (d *daemon) note(s string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.notes[s] {
		d.notes[s] = true
		d.st.Notes = append(d.st.Notes, s)
	}
}

func (d *daemon) writeStatus() {
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	d.mu.Lock()
	d.st.Updated = time.Now()
	d.st.Heap = ms.HeapInuse
	d.st.RSS = selfRSS()
	d.st.Ready = d.allStarted()
	st := d.st
	st.Projects = append([]string(nil), d.st.Projects...)
	d.mu.Unlock()
	writeStatus(&st)
}

// pollBackend has no events: every project is checked by a stat walk.
type pollBackend struct{}

func (pollBackend) Name() string                { return "polling" }
func (pollBackend) Set(roots []string) []string { return roots }
func (pollBackend) Close()                      {}
