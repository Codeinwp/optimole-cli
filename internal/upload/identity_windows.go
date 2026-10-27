package upload

import "syscall"

// pathIdentity returns the volume serial number and file index of the file at
// p, without following a final symlink: the same ID os.SameFile compares on
// Windows. It groups the aliases a path cannot show (8.3 short names, case,
// hard links, linked parents); os.SameFile still decides.
func pathIdentity(p string) (any, bool) {
	name, err := syscall.UTF16PtrFromString(p)
	if err != nil {
		return nil, false
	}
	h, err := syscall.CreateFile(name, 0,
		syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE|syscall.FILE_SHARE_DELETE,
		nil, syscall.OPEN_EXISTING,
		syscall.FILE_FLAG_BACKUP_SEMANTICS|syscall.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return nil, false
	}
	defer syscall.CloseHandle(h)
	var d syscall.ByHandleFileInformation
	if err := syscall.GetFileInformationByHandle(h, &d); err != nil {
		return nil, false
	}
	return [3]uint32{d.VolumeSerialNumber, d.FileIndexHigh, d.FileIndexLow}, true
}
