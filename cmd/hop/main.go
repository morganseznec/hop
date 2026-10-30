// Command hop opens AWS Session Manager sessions on any instance reachable from this
// machine, across every AWS profile and region.
package main

import (
	"os"

	"github.com/morganseznec/hop/internal/cli"
)

func main() {
	os.Exit(cli.Execute())
}
