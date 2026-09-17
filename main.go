// Command mcptui is an interactive terminal client for MCP servers.
package main

import (
	"os"
	"runtime/debug"
	"strings"

	"github.com/ideonate/mcptui/internal/cli"
	"github.com/ideonate/mcptui/internal/session"
)

// version is set with -ldflags "-X main.version=...".
var version = ""

func main() {
	switch {
	case version != "":
		session.Version = version
	default:
		// `go install ...@vX.Y.Z` records the module version.
		if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
			session.Version = strings.TrimPrefix(bi.Main.Version, "v")
		}
	}
	os.Exit(cli.Main(os.Args[1:]))
}
