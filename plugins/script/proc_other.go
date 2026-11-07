//go:build !unix

package script

import "os/exec"

// processGroup keeps the default: the process is killed on cancel.
func processGroup(*exec.Cmd) {}

// endGroup does nothing without process groups.
func endGroup(*exec.Cmd) {}
