package engine

import (
	"bufio"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// readHost reads memory and load from /proc, the container's memory limit from the cgroup
// (v2, then v1), the process CPU time with getrusage and the free space of the workdir.
func readHost(workdir string) hostInfo {
	var h hostInfo
	if f, err := os.Open("/proc/meminfo"); err == nil {
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			fields := strings.Fields(sc.Text())
			if len(fields) < 2 {
				continue
			}
			kb, _ := strconv.ParseUint(fields[1], 10, 64)
			switch fields[0] {
			case "MemTotal:":
				h.memTotal = kb * 1024
			case "MemAvailable:":
				h.memAvailable = kb * 1024
			}
		}
		_ = f.Close()
	}
	h.memLimit = cgroupMemoryLimit(h.memTotal)
	if b, err := os.ReadFile("/proc/loadavg"); err == nil {
		fields := strings.Fields(string(b))
		for i := 0; i < 3 && i < len(fields); i++ {
			h.load[i], _ = strconv.ParseFloat(fields[i], 64)
		}
	}
	if b, err := os.ReadFile("/proc/self/status"); err == nil {
		for _, line := range strings.Split(string(b), "\n") {
			if rest, ok := strings.CutPrefix(line, "VmRSS:"); ok {
				fields := strings.Fields(rest)
				if len(fields) > 0 {
					kb, _ := strconv.ParseUint(fields[0], 10, 64)
					h.rss = kb * 1024
				}
			}
		}
	}
	h.cpu = processCPU()
	h.workdirFree = freeSpace(workdir)
	return h
}

// cgroupMemoryLimit returns the cgroup memory limit, 0 if there is none (or if it is not lower
// than the host memory, as cgroup v1 reports "unlimited" with a huge number).
func cgroupMemoryLimit(hostTotal uint64) uint64 {
	for _, p := range []string{"/sys/fs/cgroup/memory.max", "/sys/fs/cgroup/memory/memory.limit_in_bytes"} {
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		v := strings.TrimSpace(string(b))
		if v == "max" {
			return 0
		}
		n, err := strconv.ParseUint(v, 10, 64)
		if err != nil || (hostTotal > 0 && n >= hostTotal) {
			return 0
		}
		return n
	}
	return 0
}

func processCPU() time.Duration {
	var ru syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &ru); err != nil {
		return 0
	}
	return time.Duration(ru.Utime.Nano() + ru.Stime.Nano())
}

func freeSpace(dir string) uint64 {
	if dir = existingDir(dir); dir == "" {
		return 0
	}
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0
	}
	return uint64(st.Bavail) * uint64(st.Bsize)
}
