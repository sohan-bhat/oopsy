//go:build !linux

package preload

// The preload library is Linux only: macOS mirrors with APFS clones, which
// keep old versions by themselves, and Windows has no preload mechanism.
func library() []byte { return nil }
