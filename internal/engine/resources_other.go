//go:build !linux && !darwin

package engine

// readHost reports nothing on the systems without a specific implementation: the sample keeps
// only what the Go runtime knows (CPUs, GOMAXPROCS, memory of the process, goroutines).
func readHost(string) hostInfo { return hostInfo{} }
