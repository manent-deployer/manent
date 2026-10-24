package engine

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"github.com/manent-deployer/manent/internal/service"
)

// resourceSampler samples the capacity and the use of the worker's host and process. It is
// called at registration and at every heartbeat; the process CPU is the difference between two
// consecutive samples.
type resourceSampler struct {
	workdir     string
	concurrency int

	mu       sync.Mutex
	lastCPU  time.Duration
	lastWall time.Time
}

func newResourceSampler(workdir string, concurrency int) *resourceSampler {
	return &resourceSampler{workdir: workdir, concurrency: concurrency}
}

// sample returns the current resources. Values the operating system does not provide stay zero.
func (s *resourceSampler) sample() service.WorkerResources {
	now := time.Now().UTC()
	r := service.WorkerResources{
		OS: runtime.GOOS, Arch: runtime.GOARCH,
		CPUs: runtime.NumCPU(), CPULimit: runtime.GOMAXPROCS(0),
		Goroutines: runtime.NumGoroutine(), Concurrency: s.concurrency, SampledAt: now,
	}
	h := readHost(s.workdir)
	r.MemoryTotal, r.MemoryAvailable, r.MemoryLimit = h.memTotal, h.memAvailable, h.memLimit
	r.ProcessMemory, r.WorkdirFree = h.rss, h.workdirFree
	r.Load1, r.Load5, r.Load15 = h.load[0], h.load[1], h.load[2]
	if r.ProcessMemory == 0 {
		// Without the resident memory of the OS, the memory obtained by the Go runtime.
		var m runtime.MemStats
		runtime.ReadMemStats(&m)
		r.ProcessMemory = m.Sys
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if h.cpu > 0 {
		if !s.lastWall.IsZero() && now.After(s.lastWall) && h.cpu >= s.lastCPU {
			r.ProcessCPU = round2(float64(h.cpu-s.lastCPU) / float64(now.Sub(s.lastWall)))
		}
		s.lastCPU, s.lastWall = h.cpu, now
	}
	return r
}

// sampleJSON is sample encoded for the registry.
func (s *resourceSampler) sampleJSON() json.RawMessage {
	b, err := json.Marshal(s.sample())
	if err != nil {
		return nil
	}
	return b
}

// existingDir returns dir or, if it does not exist yet (the workdir is created at the first
// deploy), its closest existing parent: the free space is that of the same file system.
func existingDir(dir string) string {
	for d := filepath.Clean(dir); ; d = filepath.Dir(d) {
		if st, err := os.Stat(d); err == nil && st.IsDir() {
			return d
		}
		if parent := filepath.Dir(d); parent == d {
			return ""
		}
	}
}

// hostInfo is what the operating system reports about host and process.
type hostInfo struct {
	memTotal, memAvailable, memLimit uint64
	rss                              uint64
	workdirFree                      uint64
	load                             [3]float64
	// cpu is the CPU time (user + system) used by the process so far.
	cpu time.Duration
}
