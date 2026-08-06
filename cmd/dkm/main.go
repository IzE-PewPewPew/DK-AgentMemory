// Command dkm is DevKuong Memories: a self-hosted memory service for AI coding
// agents, and the client that connects them to it.
//
// One binary, three roles:
//
//	dkm serve      the API, the read-only viewer, and the consolidation worker
//	dkm mcp        an MCP server on stdio, launched by each agent host
//	dkm <verb>     the human-facing client
//
// Shipping them together means the version an agent talks through and the
// version a person types are the same build. There is no second artefact to
// keep in step, and no way for a client to be newer than the server it was
// installed alongside.
package main

import (
	"os"

	"github.com/IzE-PewPewPew/DK-AgentMemory/internal/cli"
)

func main() {
	os.Exit(cli.Main(os.Args[1:]))
}
