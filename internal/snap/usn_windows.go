//go:build windows

package snap

import (
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

// On NTFS the change journal (USN journal) records every change on the
// volume. Before walking a project, Oopsy reads the journal from where its
// last walk started: when nothing touched any of the project's folders, the
// walk is skipped. This turns the stat walk of a big project into a read of a
// few journal records. Anything unexpected falls back to the walk.

const (
	fsctlQueryUsnJournal            = 0x000900F4
	fsctlReadUsnJournal             = 0x000900BB
	fsctlReadUnprivilegedUsnJournal = 0x000903AB
)

// frnMask keeps the MFT record number of a file reference and drops its
// sequence number, so references from the journal and from file handles
// compare equal.
const frnMask = 0x0000FFFFFFFFFFFF

const (
	usnReasonHardLinkChange = 0x00010000
	usnReasonClose          = 0x80000000
)

type usnJournalData struct {
	UsnJournalID    uint64
	FirstUsn        int64
	NextUsn         int64
	LowestValidUsn  int64
	MaxUsn          int64
	MaximumSize     uint64
	AllocationDelta uint64
	MinMajor        uint16
	MaxMajor        uint16
	_               [4]byte
}

type readUsnJournalData struct {
	StartUsn          int64
	ReasonMask        uint32
	ReturnOnlyOnClose uint32
	Timeout           uint64
	BytesToWaitFor    uint64
	UsnJournalID      uint64
	MinMajorVersion   uint16
	MaxMajorVersion   uint16
	_                 [4]byte
}

// usnState is what a mirror remembers about the journal.
type usnState struct {
	journal uint64
	next    int64
}

func openVolume(root string) (windows.Handle, error) {
	vol := filepath.VolumeName(root)
	if len(vol) != 2 || vol[1] != ':' {
		return windows.InvalidHandle, os.ErrInvalid
	}
	name, _ := windows.UTF16PtrFromString(`\\.\` + vol)
	for _, access := range []uint32{windows.GENERIC_READ, 0} {
		h, err := windows.CreateFile(name, access, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
			nil, windows.OPEN_EXISTING, 0, 0)
		if err == nil {
			return h, nil
		}
	}
	return windows.InvalidHandle, os.ErrPermission
}

func queryJournal(h windows.Handle) (usnJournalData, bool) {
	var d usnJournalData
	var ret uint32
	err := windows.DeviceIoControl(h, fsctlQueryUsnJournal, nil, 0, (*byte)(unsafe.Pointer(&d)), uint32(unsafe.Sizeof(d)), &ret, nil)
	return d, err == nil
}

// usnStart returns the journal position to remember before a walk.
func (m *Mirror) usnStart() (usnState, bool) {
	h, err := openVolume(m.Root)
	if err != nil {
		return usnState{}, false
	}
	defer windows.CloseHandle(h)
	d, ok := queryJournal(h)
	if !ok {
		return usnState{}, false
	}
	return usnState{journal: d.UsnJournalID, next: d.NextUsn}, true
}

func (m *Mirror) usnPath() string { return filepath.Join(m.Dir, "usn") }

// usnSave records the journal position a successful walk started at, and
// the file IDs of the project's folders.
func (m *Mirror) usnSave(st usnState) {
	ids := m.folderIDs()
	if ids == nil {
		_ = os.Remove(m.usnPath())
		return
	}
	var b strings.Builder
	b.WriteString(strconv.FormatUint(st.journal, 10) + " " + strconv.FormatInt(st.next, 10) + "\n")
	for _, id := range ids {
		b.WriteString(strconv.FormatUint(id, 16) + "\n")
	}
	_ = os.WriteFile(m.usnPath(), []byte(b.String()), 0o600)
}

// folderIDs lists the NTFS file IDs of the root and every mirrored folder.
func (m *Mirror) folderIDs() []uint64 {
	nodes, err := readIndex(m.indexPath())
	if err != nil {
		return nil
	}
	ids := make([]uint64, 0, 64)
	add := func(p string) bool {
		h, err := openNoFollow(p, 0, false)
		if err != nil {
			return false
		}
		defer windows.CloseHandle(h)
		var info windows.ByHandleFileInformation
		if windows.GetFileInformationByHandle(h, &info) != nil {
			return false
		}
		ids = append(ids, (uint64(info.FileIndexHigh)<<32|uint64(info.FileIndexLow))&frnMask)
		return true
	}
	// A file with hard links is reported under any one of its names, so
	// the mirror's own folders count as the project's too.
	if !add(m.Root) || !add(filepath.Join(m.Dir, "data")) {
		return nil
	}
	for _, n := range nodes {
		if n.kind == 'd' {
			if !add(m.origPath(n.rel)) {
				return nil
			}
			add(m.dataPath(n.rel))
		}
	}
	return ids
}

// usnClean reports that nothing touched the project's folders since the
// last walk, so this walk can be skipped.
func (m *Mirror) usnClean() bool {
	if os.Getenv("OOPS_NO_USN") != "" {
		return false
	}
	b, err := os.ReadFile(m.usnPath())
	if err != nil {
		Trace("change journal: no saved position")
		return false
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	var journal uint64
	var start int64
	if f := strings.Fields(lines[0]); len(f) == 2 {
		journal, _ = strconv.ParseUint(f[0], 10, 64)
		start, _ = strconv.ParseInt(f[1], 10, 64)
	}
	if journal == 0 {
		return false
	}
	ids := make(map[uint64]bool, len(lines))
	for _, l := range lines[1:] {
		if v, err := strconv.ParseUint(l, 16, 64); err == nil {
			ids[v] = true
		}
	}
	h, err := openVolume(m.Root)
	if err != nil {
		Trace("change journal: cannot open the volume")
		return false
	}
	defer windows.CloseHandle(h)
	d, ok := queryJournal(h)
	if !ok || d.UsnJournalID != journal || start < d.FirstUsn || start < d.LowestValidUsn {
		Trace("change journal: position no longer valid")
		return false
	}
	buf := make([]byte, 64<<10)
	for start < d.NextUsn {
		req := readUsnJournalData{StartUsn: start, ReasonMask: 0xFFFFFFFF, UsnJournalID: journal, MinMajorVersion: 2, MaxMajorVersion: 2}
		var ret uint32
		err := windows.DeviceIoControl(h, fsctlReadUnprivilegedUsnJournal, (*byte)(unsafe.Pointer(&req)), uint32(unsafe.Sizeof(req)),
			&buf[0], uint32(len(buf)), &ret, nil)
		if err != nil {
			err = windows.DeviceIoControl(h, fsctlReadUsnJournal, (*byte)(unsafe.Pointer(&req)), uint32(unsafe.Sizeof(req)),
				&buf[0], uint32(len(buf)), &ret, nil)
		}
		if err != nil || ret < 8 {
			Trace("change journal: read failed: " + fmt.Sprint(err))
			return false
		}
		next := int64(binary.LittleEndian.Uint64(buf))
		off := 8
		for off+64 <= int(ret) {
			length := int(binary.LittleEndian.Uint32(buf[off:]))
			if length < 64 || off+length > int(ret) {
				return false
			}
			if binary.LittleEndian.Uint16(buf[off+4:]) != 2 {
				return false // not a V2 record: be safe and walk
			}
			frn := binary.LittleEndian.Uint64(buf[off+8:]) & frnMask
			parent := binary.LittleEndian.Uint64(buf[off+16:]) & frnMask
			if usn := int64(binary.LittleEndian.Uint64(buf[off+24:])); usn >= d.NextUsn {
				return true
			}
			reason := binary.LittleEndian.Uint32(buf[off+40:])
			// The mirror's own hard links show up as link changes; they do
			// not change any content.
			harmless := reason&^(usnReasonHardLinkChange|usnReasonClose) == 0
			if !harmless && (ids[frn] || ids[parent]) {
				nameLen := int(binary.LittleEndian.Uint16(buf[off+56:]))
				nameOff := int(binary.LittleEndian.Uint16(buf[off+58:]))
				name := ""
				if off+nameOff+nameLen <= int(ret) {
					u := make([]uint16, nameLen/2)
					for i := range u {
						u[i] = binary.LittleEndian.Uint16(buf[off+nameOff+2*i:])
					}
					name = windows.UTF16ToString(u)
				}
				Trace(fmt.Sprintf("change journal: %s changed (reason %#x)", name, reason))
				return false
			}
			off += length
		}
		if next <= start {
			break
		}
		start = next
	}
	return true
}


// usnAdvance moves the remembered position forward after a clean check.
func (m *Mirror) usnAdvance(st usnState) {
	b, err := os.ReadFile(m.usnPath())
	if err != nil {
		return
	}
	i := strings.IndexByte(string(b), '\n')
	if i < 0 {
		return
	}
	head := strconv.FormatUint(st.journal, 10) + " " + strconv.FormatInt(st.next, 10)
	_ = os.WriteFile(m.usnPath(), append([]byte(head), b[i:]...), 0o600)
}
