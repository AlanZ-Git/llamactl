//go:build windows

package models

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

// Compile on the development host; execution is a separate Windows proof.
func TestWindowsInventorySafeOpenAndWriteShare(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "synthetic.gguf")
	if err := os.WriteFile(path, []byte("synthetic"), 0600); err != nil {
		t.Fatal(err)
	}
	f, err := safeInventoryOpen(root, "synthetic.gguf")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		t.Fatal(err)
	}
	if id := inventoryFileID(f, info); id == "unknown" || id == "" {
		t.Fatal("file identity unavailable")
	}
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		t.Fatal(err)
	}
	h, err := windows.CreateFile(p, windows.GENERIC_WRITE, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, windows.OPEN_EXISTING, 0, 0)
	if err == nil {
		windows.CloseHandle(h)
		t.Fatal("write admitted while verification handle held")
	}
	if err != windows.ERROR_SHARING_VIOLATION {
		t.Fatalf("unexpected write error: %v", err)
	}
	for _, relative := range []string{"../synthetic.gguf", "synthetic.gguf:stream"} {
		if other, err := safeInventoryOpen(root, relative); err == nil {
			other.Close()
			t.Fatalf("unsafe member admitted: %s", relative)
		}
	}
}

func TestWindowsInventoryRejectsReparse(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "target"), []byte("synthetic"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "target"), filepath.Join(root, "link")); err != nil {
		t.Skipf("Windows symlink privilege unavailable: %v", err)
	}
	if f, err := safeInventoryOpen(root, "link"); err == nil {
		f.Close()
		t.Fatal("in-root reparse followed")
	}
}
