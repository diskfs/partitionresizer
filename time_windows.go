//go:build windows

package partitionresizer

import (
	"io/fs"
	"syscall"
	"time"
)

func getAccessTime(info fs.FileInfo) time.Time {
	stat := info.Sys().(*syscall.Win32FileAttributeData)
	return time.Unix(0, stat.LastAccessTime.Nanoseconds())
}
