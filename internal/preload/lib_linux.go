//go:build linux

package preload

import (
	_ "embed"
	"runtime"
)

// Built by .github/workflows/preload.yml in manylinux2014 containers
// (glibc 2.17), so they load on any glibc based distribution since 2014.

//go:embed lib/liboopsy_preload_amd64.so
var libAMD64 []byte

//go:embed lib/liboopsy_preload_arm64.so
var libARM64 []byte

func library() []byte {
	switch runtime.GOARCH {
	case "amd64":
		return libAMD64
	case "arm64":
		return libARM64
	}
	return nil
}
