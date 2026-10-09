package watch

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// selfRSS is this process's resident memory.
func selfRSS() uint64 { return processRSS(os.Getpid()) }

// processRSS reads VmRSS from /proc/<pid>/status.
func processRSS(pid int) uint64 {
	f, err := os.Open(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		return 0
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if v, ok := strings.CutPrefix(sc.Text(), "VmRSS:"); ok {
			f := strings.Fields(v)
			if len(f) > 0 {
				kb, _ := strconv.ParseUint(f[0], 10, 64)
				return kb << 10
			}
		}
	}
	return 0
}
