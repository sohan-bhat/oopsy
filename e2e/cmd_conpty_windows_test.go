package e2e

import (
	"bytes"
	"strings"
	"sync"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// conPTY is an interactive cmd.exe on a pseudo console, so it reads what is
// typed through the console exactly as it does in a terminal window, with
// AutoRun, doskey macros and Clink all active.
type conPTY struct {
	t     *testing.T
	pc    windows.Handle
	in    windows.Handle
	out   windows.Handle
	proc  windows.Handle
	mu    sync.Mutex
	buf   bytes.Buffer
	done  chan struct{}
	start int
}

var procUpdateProcThreadAttribute = windows.NewLazySystemDLL("kernel32.dll").NewProc("UpdateProcThreadAttribute")

// startConPTY runs cmdline in dir on a new pseudo console.
func startConPTY(t *testing.T, cmdline, dir string, env []string) *conPTY {
	t.Helper()
	var ptyIn, inW, outR, ptyOut windows.Handle
	if err := windows.CreatePipe(&ptyIn, &inW, nil, 0); err != nil {
		t.Fatal(err)
	}
	if err := windows.CreatePipe(&outR, &ptyOut, nil, 0); err != nil {
		t.Fatal(err)
	}
	c := &conPTY{t: t, in: inW, out: outR, done: make(chan struct{})}
	if err := windows.CreatePseudoConsole(windows.Coord{X: 200, Y: 50}, ptyIn, ptyOut, 0, &c.pc); err != nil {
		t.Fatalf("CreatePseudoConsole: %v", err)
	}
	attrs, err := windows.NewProcThreadAttributeList(1)
	if err != nil {
		t.Fatal(err)
	}
	defer attrs.Delete()
	// The pseudo console attribute takes the handle itself as its value.
	r, _, err := procUpdateProcThreadAttribute.Call(uintptr(unsafe.Pointer(attrs.List())), 0,
		windows.PROC_THREAD_ATTRIBUTE_PSEUDOCONSOLE, uintptr(c.pc), unsafe.Sizeof(c.pc), 0, 0)
	if r == 0 {
		t.Fatalf("UpdateProcThreadAttribute: %v", err)
	}
	si := &windows.StartupInfoEx{ProcThreadAttributeList: attrs.List()}
	si.Cb = uint32(unsafe.Sizeof(*si))
	// Without this, cmd.exe would write to the test's own redirected
	// stdout instead of the pseudo console.
	si.Flags = windows.STARTF_USESTDHANDLES
	var pi windows.ProcessInformation
	cl, _ := windows.UTF16PtrFromString(cmdline)
	wd, _ := windows.UTF16PtrFromString(dir)
	block := envBlock(env)
	err = windows.CreateProcess(nil, cl, nil, nil, false,
		windows.EXTENDED_STARTUPINFO_PRESENT|windows.CREATE_UNICODE_ENVIRONMENT,
		&block[0], wd, &si.StartupInfo, &pi)
	if err != nil {
		t.Fatalf("CreateProcess %s: %v", cmdline, err)
	}
	windows.CloseHandle(pi.Thread)
	windows.CloseHandle(ptyIn)
	windows.CloseHandle(ptyOut)
	c.proc = pi.Process
	go c.read()
	t.Cleanup(c.close)
	return c
}

func envBlock(env []string) []uint16 {
	var b []uint16
	for _, kv := range env {
		u, _ := windows.UTF16FromString(kv)
		b = append(b, u...)
	}
	return append(b, 0)
}

func (c *conPTY) read() {
	defer close(c.done)
	p := make([]byte, 8192)
	for {
		var n uint32
		if err := windows.ReadFile(c.out, p, &n, nil); err != nil || n == 0 {
			return
		}
		c.mu.Lock()
		c.buf.Write(p[:n])
		c.mu.Unlock()
	}
}

// Output is everything shown so far, with escape sequences removed.
func (c *conPTY) Output() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return stripVT(c.buf.String())
}

// Type sends keystrokes. A carriage return is Enter.
func (c *conPTY) Type(s string) {
	c.t.Helper()
	var n uint32
	if err := windows.WriteFile(c.in, []byte(s), &n, nil); err != nil {
		c.t.Fatalf("typing %q: %v", s, err)
	}
}

// Wait waits until the screen shows want after everything seen so far.
func (c *conPTY) Wait(want string, timeout time.Duration) {
	c.t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		out := c.Output()
		if i := strings.Index(out[min(c.start, len(out)):], want); i >= 0 {
			c.start += i + len(want)
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	c.t.Fatalf("cmd.exe never showed %q; screen so far:\n%s", want, c.Output())
}

// Exited waits for cmd.exe to end.
func (c *conPTY) Exited(timeout time.Duration) bool {
	ev, _ := windows.WaitForSingleObject(c.proc, uint32(timeout/time.Millisecond))
	return ev == windows.WAIT_OBJECT_0
}

func (c *conPTY) close() {
	if c.proc != 0 {
		if !c.Exited(0) {
			windows.TerminateProcess(c.proc, 1)
		}
		windows.CloseHandle(c.proc)
		c.proc = 0
	}
	if c.pc != 0 {
		windows.ClosePseudoConsole(c.pc)
		c.pc = 0
	}
	windows.CloseHandle(c.in)
	select {
	case <-c.done:
	case <-time.After(5 * time.Second):
	}
	windows.CloseHandle(c.out)
}

// stripVT removes the escape sequences a pseudo console writes, and joins
// the screen into plain text.
func stripVT(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != 0x1b {
			if s[i] != '\r' {
				b.WriteByte(s[i])
			}
			continue
		}
		if i+1 < len(s) && s[i+1] == '[' {
			j := i + 2
			for j < len(s) && (s[j] < 0x40 || s[j] > 0x7e) {
				j++
			}
			// Cursor moves stand for spacing on the screen.
			if j < len(s) && (s[j] == 'C' || s[j] == 'H') {
				b.WriteByte(' ')
			}
			i = j
			continue
		}
		if i+1 < len(s) && s[i+1] == ']' {
			j := i + 2
			for j < len(s) && s[j] != 0x07 && !(s[j] == 0x1b && j+1 < len(s) && s[j+1] == '\\') {
				j++
			}
			if j < len(s) && s[j] == 0x1b {
				j++
			}
			i = j
			continue
		}
		i++
	}
	return b.String()
}
