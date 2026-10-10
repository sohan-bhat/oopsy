//go:build windows

package snap

import (
	"os"
	"path/filepath"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

// volumeRoot returns the root of p's volume (D:\ or a mounted folder) when
// it is a local fixed disk. Network shares and removable drives get none.
func volumeRoot(p string) string {
	u, err := windows.UTF16PtrFromString(longPath(p))
	if err != nil {
		return ""
	}
	buf := make([]uint16, windows.MAX_LONG_PATH)
	if windows.GetVolumePathName(u, &buf[0], uint32(len(buf))) != nil {
		return ""
	}
	root := strings.TrimPrefix(windows.UTF16ToString(buf), `\\?\`)
	r, _ := windows.UTF16PtrFromString(root)
	if windows.GetDriveType(r) != windows.DRIVE_FIXED {
		return ""
	}
	return root
}

// volumeTop is the shared .oops folder at a volume's root. Each user gets a
// private folder inside it, so one user's snapshots stay unreadable to others.
func volumeTop(root string) string { return filepath.Join(root, ".oops") }

func volumeBase(root string) string {
	sid := userSID()
	if sid == "" {
		return ""
	}
	return filepath.Join(volumeTop(root), sid)
}

func userSID() string {
	tu, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return ""
	}
	return tu.User.Sid.String()
}

func ensureVolumeBase(base string) error {
	if fi, err := os.Stat(base); err == nil && fi.IsDir() {
		return nil
	}
	top := filepath.Dir(base)
	if err := os.Mkdir(top, 0o755); err == nil {
		if u, err := windows.UTF16PtrFromString(top); err == nil {
			_ = windows.SetFileAttributes(u, windows.FILE_ATTRIBUTE_HIDDEN)
		}
	} else if !os.IsExist(err) {
		return err
	}
	// Only this user and SYSTEM, with no inherited entries: the root of a
	// data drive usually lets every user read new folders.
	sd, err := windows.SecurityDescriptorFromString("D:P(A;OICI;FA;;;" + userSID() + ")(A;OICI;FA;;;SY)")
	if err != nil {
		return err
	}
	sa := &windows.SecurityAttributes{Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), SecurityDescriptor: sd}
	u, err := windows.UTF16PtrFromString(longPath(base))
	if err != nil {
		return err
	}
	if err := windows.CreateDirectory(u, sa); err != nil && err != windows.ERROR_ALREADY_EXISTS {
		return err
	}
	return nil
}
