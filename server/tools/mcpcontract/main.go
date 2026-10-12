// Command mcpcontract writes api/mcp-tools.json from the MCP server's own tool definitions and
// policy table. With -check it compares instead and fails if the file is out of date.
//
//	go run ./tools/mcpcontract <path> [-check]
package main

import (
	"bytes"
	"flag"
	"fmt"
	"os"

	"github.com/oldsj/mainloop/server/internal/mcpserver"
)

func main() {
	check := flag.Bool("check", false, "fail if the file differs instead of writing it")
	flag.Parse()
	if flag.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: mcpcontract [-check] <path>")
		os.Exit(2)
	}
	path := flag.Arg(0)
	want, err := mcpserver.ContractJSON()
	if err != nil {
		fmt.Fprintln(os.Stderr, "mcpcontract:", err)
		os.Exit(1)
	}
	if *check {
		got, err := os.ReadFile(path) //nolint:gosec // a path the developer passed on the command line
		if err != nil || !bytes.Equal(got, want) {
			fmt.Fprintf(os.Stderr, "%s is out of date; run go generate ./internal/mcpserver\n", path)
			os.Exit(1)
		}
		return
	}
	if err := os.WriteFile(path, want, 0o644); err != nil { //nolint:gosec // a tracked, world-readable repository file
		fmt.Fprintln(os.Stderr, "mcpcontract:", err)
		os.Exit(1)
	}
}
