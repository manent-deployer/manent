// Comando manent: strumento di deploy as code, utilizzabile come CLI o come server API.
package main

import (
	"os"

	"github.com/joeg-ita/manent/internal/cli"
)

func main() {
	if err := cli.Run(os.Args[1:]); err != nil {
		// go-flags stampa già gli errori di parsing; qui serve solo il codice di uscita.
		os.Exit(1)
	}
}
