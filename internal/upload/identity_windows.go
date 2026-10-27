package upload

import (
	"path/filepath"
	"strings"
	"syscall"
)

// pathIdentity returns the volume serial number and file index of the file at
// p, without following a final symlink: the same ID os.SameFile compares on
// Windows. It groups the aliases a path cannot show (8.3 short names, case,
// hard links, linked parents); os.SameFile still decides.
func pathIdentity(p string) (any, error) {
	name, err := syscall.UTF16PtrFromString(extendedPath(p))
	if err != nil {
		return nil, err
	}
	h, err := syscall.CreateFile(name, 0,
		syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE|syscall.FILE_SHARE_DELETE,
		nil, syscall.OPEN_EXISTING,
		syscall.FILE_FLAG_BACKUP_SEMANTICS|syscall.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return nil, err
	}
	defer syscall.CloseHandle(h)
	var d syscall.ByHandleFileInformation
	if err := syscall.GetFileInformationByHandle(h, &d); err != nil {
		return nil, err
	}
	return [3]uint32{d.VolumeSerialNumber, d.FileIndexHigh, d.FileIndexLow}, nil
}

// extendedPath lets CreateFile open a path of any length, as package os does
// for its own calls: an absolute path of 248 characters or more gets the
// \\?\ prefix (\\?\UNC\ for a share). Without it, Windows versions that do
// not allow long paths refuse it. filepath.Abs applies Windows' own clean-up
// first, which the prefix would otherwise skip. A path that is already
// extended or a device path is kept as given: cleaning it could change which
// file it names ("photos." would become "photos").
func extendedPath(p string) string {
	for _, prefix := range []string{`\\?\`, `\\.\`, `\??\`} {
		if strings.HasPrefix(p, prefix) {
			return p
		}
	}
	abs, err := filepath.Abs(p)
	if err != nil || len(abs) < 248 {
		return p
	}
	switch {
	case strings.HasPrefix(abs, `\\?\`), strings.HasPrefix(abs, `\\.\`):
		return abs
	case strings.HasPrefix(abs, `\\`):
		return `\\?\UNC\` + abs[2:]
	case len(abs) >= 3 && abs[1] == ':' && abs[2] == '\\':
		return `\\?\` + abs
	}
	return p
}
