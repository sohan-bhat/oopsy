package hooks

import (
	"fmt"
	"os"
	"time"

	"github.com/sohan-bhat/oopsy/internal/snap"
)

func init() {
	if traceOn {
		snap.Trace = trace
	}
}

var (
	traceOn    = os.Getenv("OOPS_TRACE") != ""
	traceStart = time.Now()
	traceLast  = traceStart
)

// trace prints how long each hook phase took when OOPS_TRACE is set.
func trace(phase string) {
	if !traceOn {
		return
	}
	now := time.Now()
	fmt.Fprintf(os.Stderr, "oops trace: %-22s %6.2f ms (total %6.2f ms)\n", phase,
		float64(now.Sub(traceLast).Microseconds())/1000, float64(now.Sub(traceStart).Microseconds())/1000)
	traceLast = now
}
