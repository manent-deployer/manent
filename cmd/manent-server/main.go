// Comando manent-server: API, worker e operazioni di piattaforma di manent.
package main

import (
	"os"

	"github.com/joeg-ita/manent/internal/servercli"
)

func main() {
	if err := servercli.Run(os.Args[1:]); err != nil {
		// go-flags stampa già gli errori di parsing; qui serve solo il codice di uscita.
		os.Exit(1)
	}
}
