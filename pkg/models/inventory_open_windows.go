//go:build windows

package models

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
)

// Verification pins non-reparse directory/file handles, denying write/delete
// sharing for the read. These handles are NOT claimed to stay open throughout
// engine execution: compatibility with a fixed engine needs Windows evidence.
func safeInventoryOpen(root, relative string) (*os.File, error) {
	if relative != "." && (!filepath.IsLocal(relative) || strings.Contains(relative, ":")) {
		return nil, fmt.Errorf("invalid relative member")
	}
	paths := []string{filepath.Clean(root)}
	if relative != "." {
		for _, part := range strings.Split(filepath.Clean(relative), string(filepath.Separator)) {
			paths = append(paths, filepath.Join(paths[len(paths)-1], part))
		}
	}
	handles := []windows.Handle{}
	defer func() {
		for _, h := range handles {
			windows.CloseHandle(h)
		}
	}()
	var volume uint32
	for index, path := range paths {
		p, err := windows.UTF16PtrFromString(path)
		if err != nil {
			return nil, err
		}
		h, err := windows.CreateFile(p, windows.GENERIC_READ, windows.FILE_SHARE_READ, nil, windows.OPEN_EXISTING, windows.FILE_FLAG_OPEN_REPARSE_POINT|windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
		if err != nil {
			return nil, err
		}
		handles = append(handles, h)
		var info windows.ByHandleFileInformation
		if err = windows.GetFileInformationByHandle(h, &info); err != nil {
			return nil, err
		}
		if info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
			return nil, fmt.Errorf("blocked_link")
		}
		if index < len(paths)-1 && info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY == 0 {
			return nil, fmt.Errorf("not_directory")
		}
		if index == 0 {
			volume = info.VolumeSerialNumber
		} else if volume != info.VolumeSerialNumber {
			return nil, fmt.Errorf("volume_changed")
		}
		buf := make([]uint16, 32768)
		size, err := windows.GetFinalPathNameByHandle(h, &buf[0], uint32(len(buf)), 0)
		if err != nil || size >= uint32(len(buf)) {
			return nil, fmt.Errorf("final_path_unavailable")
		}
		final := windows.UTF16ToString(buf[:size])
		final = strings.TrimPrefix(final, `\\?\`)
		if strings.HasPrefix(final, `UNC\`) {
			final = `\\` + strings.TrimPrefix(final, `UNC\`)
		}
		if !strings.EqualFold(filepath.Clean(final), filepath.Clean(path)) {
			return nil, fmt.Errorf("final_path_changed")
		}
		if index == len(paths)-1 {
			handles = handles[:len(handles)-1]
			return os.NewFile(uintptr(h), path), nil
		}
	}
	return nil, fmt.Errorf("invalid member")
}
func inventoryFileID(f *os.File, _ os.FileInfo) string {
	var info windows.ByHandleFileInformation
	if windows.GetFileInformationByHandle(windows.Handle(f.Fd()), &info) != nil {
		return "unknown"
	}
	return fmt.Sprintf("%d:%d:%d", info.VolumeSerialNumber, info.FileIndexHigh, info.FileIndexLow)
}
