package watch

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"unsafe"

	"golang.org/x/sys/unix"

	"github.com/sohan-bhat/oopsy/internal/snap"
)

// inotify: one watch per folder of each project (git's folder and
// rebuildable folders are skipped), added for new folders as they appear.
// A queue overflow means events were lost, so every project gets a full
// comparison. When the kernel's watch limit is reached, the project is
// checked by a periodic stat walk instead.

const inotifyMask = unix.IN_MODIFY | unix.IN_CLOSE_WRITE | unix.IN_ATTRIB | unix.IN_CREATE | unix.IN_DELETE |
	unix.IN_MOVED_FROM | unix.IN_MOVED_TO | unix.IN_DELETE_SELF | unix.IN_MOVE_SELF | unix.IN_ONLYDIR | inDontFollow

const inDontFollow = 0x2000000

type watchDir struct{ root, path string }

type inotify struct {
	fd     int
	notify notifyFunc
	mu     sync.Mutex
	wds    map[int]watchDir
	roots  map[string]bool
	full   map[string]bool // roots that hit the watch limit
	closed chan struct{}
	done   chan struct{}
}

func newBackend(notify notifyFunc) (backend, error) {
	fd, err := unix.InotifyInit1(unix.IN_CLOEXEC | unix.IN_NONBLOCK)
	if err != nil {
		return nil, err
	}
	in := &inotify{fd: fd, notify: notify, wds: map[int]watchDir{}, roots: map[string]bool{}, full: map[string]bool{},
		closed: make(chan struct{}), done: make(chan struct{})}
	go in.read()
	return in, nil
}

func (in *inotify) Name() string { return "inotify" }

func (in *inotify) Set(roots []string) []string {
	want := map[string]bool{}
	for _, r := range roots {
		want[r] = true
	}
	in.mu.Lock()
	for r := range in.roots {
		if !want[r] {
			in.dropRoot(r)
		}
	}
	var add []string
	for _, r := range roots {
		if !in.roots[r] {
			in.roots[r] = true
			add = append(add, r)
		}
	}
	in.mu.Unlock()
	for _, r := range add {
		in.addTree(r, r)
	}
	in.mu.Lock()
	defer in.mu.Unlock()
	var poll []string
	for _, r := range roots {
		if in.full[r] {
			poll = append(poll, r)
		}
	}
	return poll
}

// dropRoot removes every watch of a project. The caller holds mu.
func (in *inotify) dropRoot(root string) {
	for wd, w := range in.wds {
		if w.root == root {
			_, _ = unix.InotifyRmWatch(in.fd, uint32(wd))
			delete(in.wds, wd)
		}
	}
	delete(in.roots, root)
	delete(in.full, root)
}

// addTree watches dir and every folder under it.
func (in *inotify) addTree(root, dir string) {
	in.mu.Lock()
	if in.full[root] || !in.roots[root] {
		in.mu.Unlock()
		return
	}
	in.mu.Unlock()
	if dir != root && snap.WatchIgnored(root, dir) {
		return
	}
	wd, err := unix.InotifyAddWatch(in.fd, dir, inotifyMask)
	if err != nil {
		if errors.Is(err, unix.ENOSPC) {
			in.mu.Lock()
			in.full[root] = true
			in.mu.Unlock()
		}
		return
	}
	in.mu.Lock()
	in.wds[wd] = watchDir{root, dir}
	in.mu.Unlock()
	ents, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range ents {
		if e.IsDir() { // DirEntry from ReadDir does not follow links
			in.addTree(root, filepath.Join(dir, e.Name()))
		}
	}
}

func (in *inotify) Close() {
	close(in.closed)
	<-in.done
	unix.Close(in.fd)
}

func (in *inotify) read() {
	defer close(in.done)
	buf := make([]byte, 64<<10)
	fds := []unix.PollFd{{Fd: int32(in.fd), Events: unix.POLLIN}}
	for {
		select {
		case <-in.closed:
			return
		default:
		}
		n, err := unix.Poll(fds, 500)
		if err != nil && !errors.Is(err, unix.EINTR) {
			return
		}
		if n <= 0 {
			continue
		}
		n, err = unix.Read(in.fd, buf)
		if err != nil || n <= 0 {
			continue
		}
		in.parse(buf[:n])
	}
}

func (in *inotify) parse(b []byte) {
	for len(b) >= unix.SizeofInotifyEvent {
		ev := (*unix.InotifyEvent)(unsafe.Pointer(&b[0]))
		size := unix.SizeofInotifyEvent + int(ev.Len)
		if size > len(b) {
			return
		}
		name := strings.TrimRight(string(b[unix.SizeofInotifyEvent:size]), "\x00")
		b = b[size:]
		if ev.Mask&unix.IN_Q_OVERFLOW != 0 {
			in.mu.Lock()
			var roots []string
			for r := range in.roots {
				roots = append(roots, r)
			}
			in.mu.Unlock()
			for _, r := range roots {
				in.notify(r, "", true)
			}
			continue
		}
		in.mu.Lock()
		w, ok := in.wds[int(ev.Wd)]
		if ev.Mask&unix.IN_IGNORED != 0 {
			delete(in.wds, int(ev.Wd))
		}
		in.mu.Unlock()
		if !ok {
			continue
		}
		p := w.path
		if name != "" {
			p = filepath.Join(w.path, name)
		}
		if ev.Mask&unix.IN_ISDIR != 0 && ev.Mask&(unix.IN_CREATE|unix.IN_MOVED_TO) != 0 {
			in.addTree(w.root, p)
		}
		in.notify(w.root, p, false)
	}
}
