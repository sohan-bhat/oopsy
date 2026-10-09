package snap

import "golang.org/x/sys/unix"

func statMtime(st *unix.Stat_t) (int64, int64) { return int64(st.Mtim.Sec), int64(st.Mtim.Nsec) }

// listDir on Linux stats each entry relative to the open folder.
func listDir(dir string) ([]dirEntry, error) { return listDirStat(dir) }
