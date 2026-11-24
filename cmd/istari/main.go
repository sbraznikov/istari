// Command istari is the single binary for the daemon, the CLI and the TUI.
package main

import (
	"os"

	"github.com/sbraznikov/istari/internal/cli"
	_ "github.com/sbraznikov/istari/internal/harness/claudecode" // registers the harness
)

func main() {
	os.Exit(cli.Execute(os.Args[1:], os.Stdout, os.Stderr))
}
