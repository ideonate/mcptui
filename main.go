// Command mcptui is an interactive terminal client for MCP servers.
package main

import (
	"os"

	"github.com/ideonate/mcptui/internal/cli"
	"github.com/ideonate/mcptui/internal/session"
)

// version is set with -ldflags "-X main.version=...".
var version = ""

func main() {
	if version != "" {
		session.Version = version
	}
	os.Exit(cli.Main(os.Args[1:]))
}
