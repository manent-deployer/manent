// Command manent: the manent client. It reads YAML descriptors and talks to the manent-server API.
package main

import (
	"os"

	"github.com/joeg-ita/manent/internal/clientcli"
)

func main() {
	if err := clientcli.Run(os.Args[1:]); err != nil {
		// go-flags already prints parsing errors; only the exit code is needed here.
		os.Exit(1)
	}
}
