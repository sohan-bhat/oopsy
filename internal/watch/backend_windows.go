package watch

import (
	"path/filepath"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
)

// ReadDirectoryChangesW: one recursive watch per project, on its own
// handle and goroutine. A read that returns no entries means the buffer
// overflowed and events were lost, so the project gets a full comparison.

const rdcwMask = windows.FILE_NOTIFY_CHANGE_FILE_NAME | windows.FILE_NOTIFY_CHANGE_DIR_NAME |
	windows.FILE_NOTIFY_CHANGE_SIZE | windows.FILE_NOTIFY_CHANGE_LAST_WRITE | windows.FILE_NOTIFY_CHANGE_ATTRIBUTES

type rdcw struct {
	notify notifyFunc
	mu     sync.Mutex
	roots  map[string]*dirWatch
}

type dirWatch struct {
	root string
	h    windows.Handle
	ov   windows.Overlapped
	done chan struct{}
	mu   sync.Mutex
	stop bool
}

func newBackend(notify notifyFunc) (backend, error) {
	return &rdcw{notify: notify, roots: map[string]*dirWatch{}}, nil
}

func (r *rdcw) Name() string { return "ReadDirectoryChangesW" }

func (r *rdcw) Set(roots []string) []string {
	want := map[string]bool{}
	for _, root := range roots {
		want[root] = true
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for root, w := range r.roots {
		if !want[root] {
			w.close()
			delete(r.roots, root)
		}
	}
	var poll []string
	for _, root := range roots {
		if r.roots[root] != nil {
			continue
		}
		w, err := r.open(root)
		if err != nil {
			poll = append(poll, root)
			continue
		}
		r.roots[root] = w
	}
	return poll
}

func (r *rdcw) open(root string) (*dirWatch, error) {
	p, err := windows.UTF16PtrFromString(root)
	if err != nil {
		return nil, err
	}
	h, err := windows.CreateFile(p, windows.FILE_LIST_DIRECTORY,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, windows.OPEN_EXISTING,
		windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OVERLAPPED, 0)
	if err != nil {
		return nil, err
	}
	ev, err := windows.CreateEvent(nil, 1, 0, nil)
	if err != nil {
		windows.CloseHandle(h)
		return nil, err
	}
	w := &dirWatch{root: root, h: h, done: make(chan struct{})}
	w.ov.HEvent = ev
	go w.run(r.notify)
	return w, nil
}

func (w *dirWatch) run(notify notifyFunc) {
	defer close(w.done)
	defer windows.CloseHandle(w.ov.HEvent)
	defer windows.CloseHandle(w.h)
	buf := make([]byte, 64<<10)
	for {
		w.mu.Lock()
		stop := w.stop
		if !stop {
			_ = windows.ResetEvent(w.ov.HEvent)
		}
		var err error
		if !stop {
			err = windows.ReadDirectoryChanges(w.h, &buf[0], uint32(len(buf)), true, rdcwMask, nil, &w.ov, 0)
		}
		w.mu.Unlock()
		if stop || err != nil {
			return
		}
		var n uint32
		if err := windows.GetOverlappedResult(w.h, &w.ov, &n, true); err != nil {
			if err == windows.ERROR_NOTIFY_ENUM_DIR {
				notify(w.root, "", true)
				continue
			}
			return // cancelled, or the folder is gone
		}
		if n == 0 {
			notify(w.root, "", true) // overflow
			continue
		}
		for off := uint32(0); off+12 <= n; {
			fi := (*windows.FileNotifyInformation)(unsafe.Pointer(&buf[off]))
			name := windows.UTF16ToString(unsafe.Slice(&fi.FileName, fi.FileNameLength/2))
			notify(w.root, filepath.Join(w.root, name), false)
			if fi.NextEntryOffset == 0 {
				break
			}
			off += fi.NextEntryOffset
		}
	}
}

func (w *dirWatch) close() {
	w.mu.Lock()
	w.stop = true
	_ = windows.CancelIoEx(w.h, &w.ov)
	w.mu.Unlock()
	<-w.done
}

func (r *rdcw) Close() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for root, w := range r.roots {
		w.close()
		delete(r.roots, root)
	}
}
