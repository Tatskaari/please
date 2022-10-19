//go:build windows
// +build windows

package core

import (
	"os"
)

type logFunc func(format string, args ...interface{})

func acquireFileLock(file *os.File, how int, levelLog logFunc) error {
	return nil
}

// ReleaseFileLock releases the lock and closes the file handle.
// Does not die on errors, at this point it wouldn't really do any good.
func ReleaseFileLock(file *os.File) {
	return
}

// AcquireSharedRepoLock acquires a shared lock on the repo lock file. The file descriptor is reused if already opened
// allowing its lock mode to be replaced. Dies if the lock cannot be successfully acquired.
func AcquireSharedRepoLock() {
	return
}

// AcquireExclusiveRepoLock acquires an exclusive lock on the repo lock file. The file descriptor is reused if already opened
// allowing its lock mode to be replaced. Dies if the lock cannot be successfully acquired.
func AcquireExclusiveRepoLock() {
	return
}

// AcquireExclusiveFileLock opens a file to acquire an exclusive lock.
// Dies if the lock cannot be successfully acquired.
func AcquireExclusiveFileLock(filePath string) *os.File {
	return nil
}
