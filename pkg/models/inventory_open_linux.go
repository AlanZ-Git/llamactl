//go:build linux

package models

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

// Walk directory handles without following even an in-root symlink. The final
// handle pins the object while hashing; this does not promise an immutable path
// during a later engine open against a malicious local writer.
func safeInventoryOpen(root, relative string) (*os.File, error) {
	if relative != "." && (!filepath.IsLocal(relative) || strings.Contains(relative, "\\")) {
		return nil, fmt.Errorf("invalid relative member")
	}
	fd, err := unix.Open(root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	if relative == "." {
		return os.NewFile(uintptr(fd), root), nil
	}
	parts := strings.Split(filepath.Clean(relative), string(filepath.Separator))
	for n, part := range parts {
		flags := unix.O_RDONLY | unix.O_NOFOLLOW | unix.O_CLOEXEC | unix.O_NONBLOCK
		if n < len(parts)-1 {
			flags |= unix.O_DIRECTORY
		}
		next, e := unix.Openat(fd, part, flags, 0)
		unix.Close(fd)
		if e != nil {
			return nil, e
		}
		fd = next
	}
	return os.NewFile(uintptr(fd), relative), nil
}
func inventoryFileID(_ *os.File, info os.FileInfo) string {
	// Stat_t is supplied by os, not x/sys.
	s, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return "unknown"
	}
	return fmt.Sprintf("%d:%d", s.Dev, s.Ino)
}
