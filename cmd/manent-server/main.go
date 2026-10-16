// Command manent-server: manent's API, workers and platform operations.
package main

import (
	"os"

	"github.com/joeg-ita/manent/internal/servercli"
)

func main() {
	if err := servercli.Run(os.Args[1:]); err != nil {
		// go-flags already prints parsing errors; only the exit code is needed here.
		os.Exit(1)
	}
}
