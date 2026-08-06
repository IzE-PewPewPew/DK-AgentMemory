package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/IzE-PewPewPew/DK-AgentMemory/internal/client"
	"github.com/IzE-PewPewPew/DK-AgentMemory/internal/logx"
	"github.com/IzE-PewPewPew/DK-AgentMemory/internal/mcp"
)

// cmdMCP runs the MCP server on stdio.
//
// This is what every local agent host launches. The single hard rule is that
// stdout carries JSON-RPC and nothing else: one stray line of logging, one
// warning, one fmt.Println left behind, and the host disconnects with a parse
// error that surfaces to the user as "the memory server has no tools".
func cmdMCP(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("mcp", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	dir := fs.String("dir", "", "working directory used to detect the project; the process CWD by default")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	// Everything that might print is redirected before anything runs.
	client.SetWarningOutput(os.Stderr)
	Out = os.Stderr
	Err = os.Stderr

	c, err := client.New()
	if err != nil {
		// The host cannot show this, so make it a protocol-level error rather
		// than a silent absence: the agent is told the tools exist and why the
		// calls fail, which is what gets the user to run `dkm login`.
		fmt.Fprintf(os.Stderr, "dkm mcp: %v\n", err)
		return 1
	}

	workdir := *dir
	if workdir == "" {
		workdir, _ = os.Getwd()
	}

	logger := logx.Discard()
	if os.Getenv("DKM_MCP_DEBUG") != "" {
		// Debug logging goes to stderr only. Hosts commonly surface stderr in a
		// log panel, which makes this useful without touching the transport.
		if l, err := logx.New("debug", "text", os.Stderr); err == nil {
			logger = l
		}
	}

	srv := mcp.NewServer(client.NewMCPBackend(c, workdir), logger)
	if err := srv.ServeStdio(ctx, os.Stdin, os.Stdout); err != nil && err != io.EOF {
		fmt.Fprintf(os.Stderr, "dkm mcp: %v\n", err)
		return 1
	}
	return 0
}
