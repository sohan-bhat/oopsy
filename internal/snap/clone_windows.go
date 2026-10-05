//go:build windows

package snap

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
)

// cloneMethod names the copy on write mechanism on this platform: ReFS block
// cloning, which Dev Drive volumes also use.
const cloneMethod = "block clone"

const fileSupportsBlockRefcounting = 0x08000000

// duplicateExtentsData is DUPLICATE_EXTENTS_DATA.
type duplicateExtentsData struct {
	FileHandle       windows.Handle
	SourceFileOffset int64
	TargetFileOffset int64
	ByteCount        int64
}

// integrityInfo is FSCTL_GET_INTEGRITY_INFORMATION_BUFFER.
type integrityInfo struct {
	ChecksumAlgorithm        uint16
	Reserved                 uint16
	Flags                    uint32
	ChecksumChunkSizeInBytes uint32
	ClusterSizeInBytes       uint32
}

// setIntegrityInfo is FSCTL_SET_INTEGRITY_INFORMATION_BUFFER.
type setIntegrityInfo struct {
	ChecksumAlgorithm uint16
	Reserved          uint16
	Flags             uint32
}

var blockCloneCache sync.Map // volume serial -> bool

// supportsBlockClone reports whether the volume holding p can share extents
// between files (ReFS and Dev Drive).
func supportsBlockClone(p string) bool {
	h, err := openNoFollow(p, 0, true)
	if err != nil {
		return false
	}
	defer windows.CloseHandle(h)
	var info windows.ByHandleFileInformation
	if windows.GetFileInformationByHandle(h, &info) == nil {
		if v, ok := blockCloneCache.Load(info.VolumeSerialNumber); ok {
			return v.(bool)
		}
	}
	var flags uint32
	ok := windows.GetVolumeInformationByHandle(h, nil, 0, nil, nil, &flags, nil, 0) == nil &&
		flags&fileSupportsBlockRefcounting != 0
	blockCloneCache.Store(info.VolumeSerialNumber, ok)
	return ok
}

// cloneTree block clones every file in src into dst. It returns errNoClone
// before writing anything when the volume cannot do it.
func cloneTree(src, dst string) ([]Skip, error) {
	if forceCopy {
		return nil, errNoClone
	}
	parent := filepath.Dir(dst)
	if !supportsBlockClone(src) || !sameDevice(src, parent) {
		return nil, errNoClone
	}
	skipped, err := copyTree(src, dst, blockCloneFile)
	if errors.Is(err, errNoClone) {
		_ = forceRemove(dst)
	}
	return skipped, err
}

// cloneFile is the per file copy used by the copy fallback: a block clone
// when the volume allows it, else a plain copy.
func cloneFile(src, dst string, fi os.FileInfo) error {
	if !forceCopy && supportsBlockClone(src) && sameDevice(src, filepath.Dir(dst)) {
		if err := blockCloneFile(src, dst, fi); err == nil {
			return nil
		}
		os.Remove(dst)
	}
	return plainCopy(src, dst, fi)
}

// blockCloneFile shares src's extents with a new file dst. The clone range
// has to be cluster aligned, so dst is sized up to a whole cluster first and
// trimmed to the real length afterwards.
func blockCloneFile(src, dst string, fi os.FileInfo) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	ok := false
	defer func() {
		out.Close()
		if !ok {
			os.Remove(dst)
		}
	}()
	size := fi.Size()
	if size == 0 {
		ok = true
		return nil
	}
	hin, hout := windows.Handle(in.Fd()), windows.Handle(out.Fd())
	var ret uint32
	var ii integrityInfo
	if err := windows.DeviceIoControl(hin, windows.FSCTL_GET_INTEGRITY_INFORMATION, nil, 0,
		(*byte)(unsafe.Pointer(&ii)), uint32(unsafe.Sizeof(ii)), &ret, nil); err != nil {
		return errNoClone
	}
	// Both files must agree on integrity streams for the clone to work.
	set := setIntegrityInfo{ChecksumAlgorithm: ii.ChecksumAlgorithm, Flags: ii.Flags}
	_ = windows.DeviceIoControl(hout, windows.FSCTL_SET_INTEGRITY_INFORMATION,
		(*byte)(unsafe.Pointer(&set)), uint32(unsafe.Sizeof(set)), nil, 0, &ret, nil)
	if d := winData(fi); d != nil && d.FileAttributes&windows.FILE_ATTRIBUTE_SPARSE_FILE != 0 {
		_ = windows.DeviceIoControl(hout, windows.FSCTL_SET_SPARSE, nil, 0, nil, 0, &ret, nil)
	}
	cluster := int64(ii.ClusterSizeInBytes)
	if cluster <= 0 {
		cluster = 4096
	}
	rounded := (size + cluster - 1) / cluster * cluster
	if err := windows.Ftruncate(hout, rounded); err != nil {
		return err
	}
	const chunk = 1 << 30 // each clone call must stay under 4 GB
	for off := int64(0); off < rounded; off += chunk {
		n := min(int64(chunk), rounded-off)
		d := duplicateExtentsData{FileHandle: hin, SourceFileOffset: off, TargetFileOffset: off, ByteCount: n}
		if err := windows.DeviceIoControl(hout, windows.FSCTL_DUPLICATE_EXTENTS_TO_FILE,
			(*byte)(unsafe.Pointer(&d)), uint32(unsafe.Sizeof(d)), nil, 0, &ret, nil); err != nil {
			if errors.Is(err, windows.ERROR_INVALID_FUNCTION) || errors.Is(err, windows.ERROR_NOT_SUPPORTED) {
				return errNoClone
			}
			return err
		}
	}
	if rounded != size {
		if err := windows.Ftruncate(hout, size); err != nil {
			return err
		}
	}
	ok = true
	return nil
}
