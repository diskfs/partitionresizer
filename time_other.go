//go:build linux || darwin || freebsd || netbsd || openbsd

package partitionresizer

import (
	"io/fs"
	"time"

	"golang.org/x/sys/unix"
)

func getAccessTime(info fs.FileInfo) time.Time {
	stat := info.Sys().(*unix.Stat_t)
	return time.Unix(stat.Atim.Sec, stat.Atim.Nsec)
}
