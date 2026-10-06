//go:build !windows

package e2e

func winMeta(string) (uint32, int64) { return 0, 0 }

func setWinAttrs(string, uint32) {}
