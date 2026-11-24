// Command istari is the single binary for the daemon, the CLI and the TUI.
package main

import (
	"os"

	"github.com/sbraznikov/istari/internal/cli"
)

func main() {
	os.Exit(cli.Execute(os.Args[1:], os.Stdout, os.Stderr))
}
