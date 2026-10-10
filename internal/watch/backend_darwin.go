package watch

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"unsafe"

	"github.com/ebitengine/purego"
)

// FSEvents without cgo: CoreServices and CoreFoundation are loaded with
// purego only when the daemon starts, so no other oops command pays for
// them. One stream covers every project. It runs on a CFRunLoop on its own
// locked OS thread; changing the projects stops the run loop, and the
// thread makes a new stream and runs it again.

var (
	cfStringCreate   func(alloc uintptr, s string, enc uint32) uintptr
	cfArrayCreate    func(alloc uintptr, vals *uintptr, n int, cb uintptr) uintptr
	cfRunLoopCurrent func() uintptr
	cfRunLoopRun     func()
	cfRunLoopStop    func(rl uintptr)
	cfRelease        func(p uintptr)
	fsCreate         func(alloc, cb, ctx, paths uintptr, since uint64, latency float64, flags uint32) uintptr
	fsSchedule       func(s, rl, mode uintptr)
	fsStart          func(s uintptr) bool
	fsStop           func(s uintptr)
	fsInvalidate     func(s uintptr)
	fsRelease        func(s uintptr)
	procRusage       func(pid int32, flavor int32, buf unsafe.Pointer) int32

	arrayCallbacks uintptr
	defaultMode    uintptr
	loadOnce       sync.Once
	loadErr        error
	callback       uintptr
	current        *fsevents // the stream the callback reports to
)

const (
	utf8Encoding       = 0x08000100
	eventIDSinceNow    = 0xFFFFFFFFFFFFFFFF
	flagNoDefer        = 0x02
	flagWatchRoot      = 0x04
	flagFileEvents     = 0x10
	evMustScanSubDirs  = 0x01
	evUserDropped      = 0x02
	evKernelDropped    = 0x04
	evRootChanged      = 0x20
	latencySeconds     = 0.05
	rusageInfoV0       = 0
	rusageResidentSize = 64 // offset of ri_resident_size in rusage_info_v0
)

func load() error {
	loadOnce.Do(func() {
		cf, err := purego.Dlopen("/System/Library/Frameworks/CoreFoundation.framework/CoreFoundation", purego.RTLD_NOW|purego.RTLD_GLOBAL)
		if err != nil {
			loadErr = err
			return
		}
		cs, err := purego.Dlopen("/System/Library/Frameworks/CoreServices.framework/CoreServices", purego.RTLD_NOW|purego.RTLD_GLOBAL)
		if err != nil {
			loadErr = err
			return
		}
		purego.RegisterLibFunc(&cfStringCreate, cf, "CFStringCreateWithCString")
		purego.RegisterLibFunc(&cfArrayCreate, cf, "CFArrayCreate")
		purego.RegisterLibFunc(&cfRunLoopCurrent, cf, "CFRunLoopGetCurrent")
		purego.RegisterLibFunc(&cfRunLoopRun, cf, "CFRunLoopRun")
		purego.RegisterLibFunc(&cfRunLoopStop, cf, "CFRunLoopStop")
		purego.RegisterLibFunc(&cfRelease, cf, "CFRelease")
		purego.RegisterLibFunc(&fsCreate, cs, "FSEventStreamCreate")
		purego.RegisterLibFunc(&fsSchedule, cs, "FSEventStreamScheduleWithRunLoop")
		purego.RegisterLibFunc(&fsStart, cs, "FSEventStreamStart")
		purego.RegisterLibFunc(&fsStop, cs, "FSEventStreamStop")
		purego.RegisterLibFunc(&fsInvalidate, cs, "FSEventStreamInvalidate")
		purego.RegisterLibFunc(&fsRelease, cs, "FSEventStreamRelease")
		if arrayCallbacks, err = purego.Dlsym(cf, "kCFTypeArrayCallBacks"); err != nil {
			loadErr = err
			return
		}
		mode, err := purego.Dlsym(cf, "kCFRunLoopDefaultMode")
		if err != nil {
			loadErr = err
			return
		}
		defaultMode = *(*uintptr)(ptr(mode))
		callback = purego.NewCallback(onEvents)
	})
	return loadErr
}

type fsevents struct {
	notify notifyFunc
	mu     sync.Mutex
	roots  []string          // as given
	real   map[string]string // resolved root -> root as given
	rl     uintptr
	update chan []string
	ready  chan struct{}
	closed bool
	done   chan struct{}
}

func newBackend(notify notifyFunc) (backend, error) {
	if err := load(); err != nil {
		return nil, err
	}
	f := &fsevents{notify: notify, update: make(chan []string, 1), ready: make(chan struct{}, 1), done: make(chan struct{})}
	current = f
	go f.loop()
	return f, nil
}

func (f *fsevents) Name() string { return "FSEvents" }

func (f *fsevents) Set(roots []string) []string {
	real := map[string]string{}
	for _, r := range roots {
		rr, err := filepath.EvalSymlinks(r)
		if err != nil {
			rr = r
		}
		real[rr] = r
	}
	f.mu.Lock()
	f.roots, f.real = roots, real
	rl := f.rl
	f.mu.Unlock()
	select {
	case f.update <- roots:
	default:
	}
	if rl != 0 {
		cfRunLoopStop(rl)
	}
	<-f.ready // the new stream is running
	return nil
}

func (f *fsevents) Close() {
	f.mu.Lock()
	f.closed = true
	rl := f.rl
	f.mu.Unlock()
	close(f.update)
	if rl != 0 {
		cfRunLoopStop(rl)
	}
	<-f.done
}

// loop owns the run loop thread.
func (f *fsevents) loop() {
	runtime.LockOSThread()
	defer close(f.done)
	for roots := range f.update {
		// Take the newest list if several came in.
		for more := true; more; {
			select {
			case r, ok := <-f.update:
				if !ok {
					return
				}
				roots = r
			default:
				more = false
			}
		}
		if len(roots) == 0 {
			f.ready <- struct{}{}
			continue
		}
		f.mu.Lock()
		var paths []string
		for rr := range f.real {
			paths = append(paths, rr)
		}
		f.mu.Unlock()
		strs := make([]uintptr, len(paths))
		for i, p := range paths {
			strs[i] = cfStringCreate(0, p, utf8Encoding)
		}
		arr := cfArrayCreate(0, &strs[0], len(strs), arrayCallbacks)
		for _, s := range strs {
			cfRelease(s)
		}
		stream := fsCreate(0, callback, 0, arr, eventIDSinceNow, latencySeconds, flagNoDefer|flagWatchRoot|flagFileEvents)
		cfRelease(arr)
		rl := cfRunLoopCurrent()
		fsSchedule(stream, rl, defaultMode)
		fsStart(stream)
		f.mu.Lock()
		f.rl = rl
		closed := f.closed
		f.mu.Unlock()
		f.ready <- struct{}{}
		for !closed {
			cfRunLoopRun() // until Set or Close stops it
			f.mu.Lock()
			closed = f.closed
			f.mu.Unlock()
			if len(f.update) > 0 {
				break
			}
		}
		f.mu.Lock()
		f.rl = 0
		f.mu.Unlock()
		fsStop(stream)
		fsInvalidate(stream)
		fsRelease(stream)
	}
}

// onEvents is the FSEventStreamCallback. With file events and no CF types,
// eventPaths is an array of C strings.
func onEvents(stream, info, n, paths, flags, ids uintptr) {
	f := current
	if f == nil || n == 0 {
		return
	}
	ps := unsafe.Slice((**byte)(ptr(paths)), n)
	fl := unsafe.Slice((*uint32)(ptr(flags)), n)
	f.mu.Lock()
	real := f.real
	f.mu.Unlock()
	for i := range ps {
		p := goString(ps[i])
		root, ok := rootOf(real, p)
		if !ok {
			continue
		}
		full := fl[i]&(evMustScanSubDirs|evUserDropped|evKernelDropped|evRootChanged) != 0
		f.notify(root.given, filepath.Join(root.given, strings.TrimPrefix(p, root.real)), full)
	}
}

type rootPair struct{ real, given string }

// rootOf finds the project a reported path belongs to.
func rootOf(real map[string]string, p string) (rootPair, bool) {
	best := rootPair{}
	for rr, given := range real {
		if (p == rr || strings.HasPrefix(p, rr+"/")) && len(rr) > len(best.real) {
			best = rootPair{rr, given}
		}
	}
	return best, best.real != ""
}

// ptr turns an address C handed over into a pointer.
func ptr(u uintptr) unsafe.Pointer { return *(*unsafe.Pointer)(unsafe.Pointer(&u)) }

func goString(p *byte) string {
	if p == nil {
		return ""
	}
	n := 0
	for *(*byte)(unsafe.Add(unsafe.Pointer(p), n)) != 0 {
		n++
	}
	return string(unsafe.Slice(p, n))
}

// selfRSS is this process's resident memory.
func selfRSS() uint64 { return processRSS(os.Getpid()) }

// processRSS asks the kernel for a process's resident size through
// proc_pid_rusage.
func processRSS(pid int) uint64 {
	if procRusage == nil {
		lib, err := purego.Dlopen("/usr/lib/libSystem.B.dylib", purego.RTLD_NOW|purego.RTLD_GLOBAL)
		if err != nil {
			return 0
		}
		purego.RegisterLibFunc(&procRusage, lib, "proc_pid_rusage")
	}
	var buf [128]byte
	if procRusage(int32(pid), rusageInfoV0, unsafe.Pointer(&buf[0])) != 0 {
		return 0
	}
	return *(*uint64)(unsafe.Pointer(&buf[rusageResidentSize]))
}
