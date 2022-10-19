//go:build !windows
// +build !windows

package fs

import "os"

// IsSameFile returns true if two filenames describe the same underlying file
// (i.e. inode for Unix and potentially file path names for other OS's)
func IsSameFile(a, b string) bool {
	i1, err1 := getFileInfo(a)
	i2, err2 := getFileInfo(b)
	return err1 == nil && err2 == nil && os.SameFile(i1, i2)
}

// getFileInfo returns the FileInfo of a file.
func getFileInfo(filename string) (os.FileInfo, error) {
	fi, err := os.Stat(filename)
	if err != nil {
		return nil, err
	}
	return fi, nil
}
