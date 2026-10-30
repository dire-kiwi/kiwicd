// Command kiwicd inspects and starts runs and configures secrets and
// variables on a kiwicd server, and serves the same to agents over MCP:
//
//	kiwicd login --url https://ci.example.com
//	kiwicd run list --repo owner/name
//	kiwicd run start owner/name deploy.yml --ref main --watch
//	echo -n "$TOKEN" | kiwicd secret set DEPLOY_TOKEN --repo owner/name
//	kiwicd mcp
//
// See kiwicd help.
package main

import (
	"os"

	"github.com/dire-kiwi/kiwicd/internal/cli"
)

var version = "dev"

func main() {
	os.Exit(cli.Main(os.Args[1:], version))
}
