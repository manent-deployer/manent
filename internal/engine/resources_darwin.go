package engine

import (
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// readHost reads the host memory with sysctl, the process CPU time with getrusage and the
// free space of the workdir. macOS has no container limits, nor a cheap figure for the free
// memory and the current resident memory (ru_maxrss is the peak): those stay zero, and the
// process memory is the one obtained by the Go runtime.
func readHost(workdir string) hostInfo {
	var h hostInfo
	if n, err := unix.SysctlUint64("hw.memsize"); err == nil {
		h.memTotal = n
	}
	var ru syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &ru); err == nil {
		h.cpu = time.Duration(ru.Utime.Nano() + ru.Stime.Nano())
	}
	if workdir = existingDir(workdir); workdir != "" {
		var st syscall.Statfs_t
		if err := syscall.Statfs(workdir, &st); err == nil {
			h.workdirFree = uint64(st.Bavail) * uint64(st.Bsize)
		}
	}
	return h
}
