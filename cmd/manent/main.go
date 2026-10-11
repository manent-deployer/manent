// Comando manent: client di manent. Legge descrittori YAML e parla con l'API di manent-server.
package main

import (
	"os"

	"github.com/joeg-ita/manent/internal/clientcli"
)

func main() {
	if err := clientcli.Run(os.Args[1:]); err != nil {
		// go-flags stampa già gli errori di parsing; qui serve solo il codice di uscita.
		os.Exit(1)
	}
}
