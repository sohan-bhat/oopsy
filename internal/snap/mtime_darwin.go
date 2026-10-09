package snap

import (
	"encoding/binary"
	"os"
	"unsafe"

	"golang.org/x/sys/unix"
)

func statMtime(st *unix.Stat_t) (int64, int64) { return st.Mtim.Sec, st.Mtim.Nsec }

// getattrlistbulk returns names, types, times, modes and sizes for a whole
// folder in one or two system calls, instead of one stat per file. That
// halves the walk on APFS.
const (
	attrBitMapCount      = 5
	attrCmnName          = 0x00000001
	attrCmnObjType       = 0x00000008
	attrCmnModTime       = 0x00000400
	attrCmnAccessMask    = 0x00020000
	attrCmnReturnedAttrs = 0x80000000
	attrFileDataLength   = 0x00000200
	vreg, vdir, vlnk     = 1, 2, 5
)

type attrList struct {
	bitmapCount uint16
	reserved    uint16
	commonAttr  uint32
	volAttr     uint32
	dirAttr     uint32
	fileAttr    uint32
	forkAttr    uint32
}

func listDir(dir string) ([]dirEntry, error) {
	f, err := os.Open(dir)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	al := attrList{bitmapCount: attrBitMapCount,
		commonAttr: attrCmnReturnedAttrs | attrCmnName | attrCmnObjType | attrCmnModTime | attrCmnAccessMask,
		fileAttr:   attrFileDataLength}
	buf := make([]byte, 128<<10)
	var out []dirEntry
	le := binary.LittleEndian
	for {
		n, _, errno := unix.Syscall6(unix.SYS_GETATTRLISTBULK, f.Fd(), uintptr(unsafe.Pointer(&al)),
			uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)), 0, 0)
		if errno != 0 {
			if out == nil {
				return listDirStat(dir)
			}
			return out, nil
		}
		if n == 0 {
			return out, nil
		}
		off := 0
		for i := 0; i < int(n); i++ {
			rec := buf[off:]
			length := int(le.Uint32(rec))
			if length <= 0 || off+length > len(buf) {
				return listDirStat(dir)
			}
			returnedCommon := le.Uint32(rec[4:])
			returnedFile := le.Uint32(rec[16:])
			p := 24 // length, then the five returned attribute words
			var e dirEntry
			if returnedCommon&attrCmnName != 0 {
				nameOff := int(int32(le.Uint32(rec[p:])))
				nameLen := int(le.Uint32(rec[p+4:]))
				start := p + nameOff
				if nameLen > 0 && start+nameLen <= length {
					e.name = string(rec[start : start+nameLen-1]) // drop the NUL
				}
				p += 8
			}
			var objType uint32
			if returnedCommon&attrCmnObjType != 0 {
				objType = le.Uint32(rec[p:])
				p += 4
			}
			if returnedCommon&attrCmnModTime != 0 {
				e.mtime = int64(le.Uint64(rec[p:]))*1e9 + int64(le.Uint64(rec[p+8:]))
				p += 16
			}
			if returnedCommon&attrCmnAccessMask != 0 {
				e.mode = unixMode(le.Uint32(rec[p:]))
				p += 4
			}
			if returnedFile&attrFileDataLength != 0 {
				e.size = int64(le.Uint64(rec[p:]))
			}
			switch objType {
			case vreg:
				e.kind = 'f'
			case vdir:
				e.kind = 'd'
			case vlnk:
				e.kind = 'l'
			}
			if e.name != "" {
				out = append(out, e)
			}
			off += length
		}
	}
}
