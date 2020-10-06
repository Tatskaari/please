// Package fs provides various filesystem helpers.
package fs

import (
	"io"
	"io/ioutil"
	"os"
	"path"

	"gopkg.in/op/go-logging.v1"
)

var log = logging.MustGetLogger("fs")

// DirPermissions are the default permission bits we apply to directories.
const DirPermissions = os.ModeDir | 0775

// EnsureDir ensures that the directory of the given file has been created.
func EnsureDir(filename string) error {
	dir := path.Dir(filename)
	err := os.MkdirAll(dir, DirPermissions)
	if err != nil && FileExists(dir) {
		// It looks like this is a file and not a directory. Attempt to remove it; this can
		// happen in some cases if you change a rule from outputting a file to a directory.
		log.Warning("Attempting to remove file %s; a subdirectory is required", dir)
		if err2 := os.Remove(dir); err2 == nil {
			err = os.MkdirAll(dir, DirPermissions)
		} else {
			log.Error("%s", err2)
		}
	}
	return err
}

// PathExists returns true if the given path exists, as a file or a directory.
func PathExists(filename string) bool {
	_, err := os.Lstat(filename)
	return err == nil
}

// FileExists returns true if the given path exists and is a file.
func FileExists(filename string) bool {
	info, err := os.Lstat(filename)
	return err == nil && !info.IsDir()
}

// IsSymlink returns true if the given path exists and is a symlink.
func IsSymlink(filename string) bool {
	info, err := os.Lstat(filename)
	return err == nil && (info.Mode()&os.ModeSymlink) != 0
}


// CopyFile copies a file from 'from' to 'to', with an attempt to perform a copy & rename
// to avoid chaos if anything goes wrong partway.
func CopyFile(from string, to string, mode os.FileMode) error {
	fromFile, err := os.Open(from)
	if err != nil {
		return err
	}
	defer fromFile.Close()
	return WriteFile(fromFile, to, mode)
}

// WriteFile writes data from a reader to the file named 'to', with an attempt to perform
// a copy & rename to avoid chaos if anything goes wrong partway.
func WriteFile(fromFile io.Reader, to string, mode os.FileMode) error {
	if err := os.RemoveAll(to); err != nil {
		return err
	}
	dir, file := path.Split(to)
	if err := os.MkdirAll(dir, DirPermissions); err != nil {
		return err
	}
	tempFile, err := ioutil.TempFile(dir, file)
	if err != nil {
		return err
	}
	if _, err := io.Copy(tempFile, fromFile); err != nil {
		return err
	}
	if err := tempFile.Close(); err != nil {
		return err
	}
	// OK, now file is written; adjust permissions appropriately.
	if mode == 0 {
		mode = 0664
	}
	if err := os.Chmod(tempFile.Name(), mode); err != nil {
		return err
	}
	// And move it to its final destination.
	return os.Rename(tempFile.Name(), to)
}

// IsDirectory checks if a given path is a directory
func IsDirectory(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

// IsPackage returns true if the given directory name is a package (i.e. contains a build file)
func IsPackage(buildFileNames []string, name string) bool {
	for _, buildFileName := range buildFileNames {
		if FileExists(path.Join(name, buildFileName)) {
			return true
		}
	}
	return false
}
