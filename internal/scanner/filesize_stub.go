//go:build !windows

package scanner

import "io/fs"

func allocatedFileSize(_ string, logical int64) (int64, error) { return logical, nil }
func isReparsePoint(_ string, _ fs.DirEntry) bool              { return false }
