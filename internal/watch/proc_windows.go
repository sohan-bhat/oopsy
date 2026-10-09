//go:build windows

package watch

import (
	"errors"
	"os"
	"path/filepath"
	"unsafe"

	"golang.org/x/sys/windows"
)

// singleInstance takes the daemon lock, so only one runs per store.
func singleInstance() (func(), error) {
	f, err := os.OpenFile(filepath.Join(Dir(), "daemon.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	ol := new(windows.Overlapped)
	if err := windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, ol); err != nil {
		f.Close()
		return nil, errors.New("oops watch is already running")
	}
	return func() { f.Close() }, nil
}

// alive reports whether a process with this pid is still running.
func alive(pid int) bool {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return errors.Is(err, windows.ERROR_ACCESS_DENIED)
	}
	defer windows.CloseHandle(h)
	var code uint32
	if windows.GetExitCodeProcess(h, &code) != nil {
		return false
	}
	return code == 259 // STILL_ACTIVE
}

// stopPID ends a daemon started without the task scheduler.
func stopPID(pid int) error {
	h, err := windows.OpenProcess(windows.PROCESS_TERMINATE, false, uint32(pid))
	if err != nil {
		return err
	}
	defer windows.CloseHandle(h)
	return windows.TerminateProcess(h, 0)
}

var (
	kernel32         = windows.NewLazySystemDLL("kernel32.dll")
	getConsoleWindow = kernel32.NewProc("GetConsoleWindow")
	getConsoleProcs  = kernel32.NewProc("GetConsoleProcessList")
	freeConsole      = kernel32.NewProc("FreeConsole")
	user32           = windows.NewLazySystemDLL("user32.dll")
	showWindow       = user32.NewProc("ShowWindow")
	psapi            = windows.NewLazySystemDLL("psapi.dll")
	getProcessMemory = psapi.NewProc("GetProcessMemoryInfo")
)

// hideConsole hides and leaves a console window that only this process
// uses, such as the one a logon task opens when conhost cannot run it
// headless.
func hideConsole() {
	var pids [4]uint32
	n, _, _ := getConsoleProcs.Call(uintptr(unsafe.Pointer(&pids[0])), 4)
	if n != 1 {
		return // a terminal someone is using, or no console
	}
	if hwnd, _, _ := getConsoleWindow.Call(); hwnd != 0 {
		showWindow.Call(hwnd, 0) // SW_HIDE
	}
	freeConsole.Call()
}

type processMemoryCounters struct {
	CB                         uint32
	PageFaultCount             uint32
	PeakWorkingSetSize         uintptr
	WorkingSetSize             uintptr
	QuotaPeakPagedPoolUsage    uintptr
	QuotaPagedPoolUsage        uintptr
	QuotaPeakNonPagedPoolUsage uintptr
	QuotaNonPagedPoolUsage     uintptr
	PagefileUsage              uintptr
	PeakPagefileUsage          uintptr
}

// selfRSS is the working set of this process.
func selfRSS() uint64 { return processRSS(uint32(os.Getpid())) }

// processRSS is the working set of a process, or 0.
func processRSS(pid uint32) uint64 {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return 0
	}
	defer windows.CloseHandle(h)
	var c processMemoryCounters
	c.CB = uint32(unsafe.Sizeof(c))
	if r, _, _ := getProcessMemory.Call(uintptr(h), uintptr(unsafe.Pointer(&c)), uintptr(c.CB)); r == 0 {
		return 0
	}
	return uint64(c.WorkingSetSize)
}
