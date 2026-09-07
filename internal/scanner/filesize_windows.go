//go:build windows

package scanner

import (
	"fmt"
	"io/fs"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	kernel32                  = windows.NewLazySystemDLL("kernel32.dll")
	procGetCompressedFileSize = kernel32.NewProc("GetCompressedFileSizeW")
)

func allocatedFileSize(path string, _ int64) (int64, error) {
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, err
	}
	var high uint32
	low, _, callErr := procGetCompressedFileSize.Call(uintptr(unsafe.Pointer(name)), uintptr(unsafe.Pointer(&high)))
	if uint32(low) == 0xffffffff {
		if errno, ok := callErr.(syscall.Errno); ok && errno != 0 {
			return 0, fmt.Errorf("GetCompressedFileSizeW: %w", errno)
		}
	}
	return int64(uint64(high)<<32 | uint64(uint32(low))), nil
}

func isReparsePoint(_ string, d fs.DirEntry) bool {
	info, err := d.Info()
	if err != nil {
		return false
	}
	data, ok := info.Sys().(*syscall.Win32FileAttributeData)
	return ok && data.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0
}
