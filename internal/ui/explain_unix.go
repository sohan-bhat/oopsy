//go:build !windows

package ui

import (
	"errors"
	"os"
	"os/user"
	"strconv"
	"syscall"
)

func ownerName(p string) string {
	fi, err := os.Lstat(p)
	if err != nil {
		return ""
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return ""
	}
	u, err := user.LookupId(strconv.Itoa(int(st.Uid)))
	if err != nil {
		return strconv.Itoa(int(st.Uid))
	}
	return u.Username
}

func currentUser() string {
	if u, err := user.Current(); err == nil {
		return u.Username
	}
	return "$USER"
}

func diskFull(err error) bool { return errors.Is(err, syscall.ENOSPC) || errors.Is(err, syscall.EDQUOT) }

func readOnly(err error) bool { return errors.Is(err, syscall.EROFS) }
