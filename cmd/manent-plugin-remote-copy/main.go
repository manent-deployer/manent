// Command manent-plugin-remote-copy is the remote-copy plugin shipped as an external binary.
// It shows that the same plugin.Factory can be compiled into manent or served as a separate
// process. Configuration:
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
