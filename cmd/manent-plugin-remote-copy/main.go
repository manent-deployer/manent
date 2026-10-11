// Command manent-plugin-remote-copy è il plugin remote-copy distribuito come binario esterno.
// Mostra che la stessa plugin.Factory può essere compilata dentro manent o servita come processo
// separato. Configurazione:
//
//	plugins:
//	  remote-copy-ext:
//	    external:
//	      command: bin/manent-plugin-remote-copy
//	    artifact_types: [application]
package main

import (
	"github.com/joeg-ita/manent/pkg/plugin/external"
	"github.com/joeg-ita/manent/plugins/remotecopy"
)

func main() {
	external.Serve(remotecopy.Factory)
}
