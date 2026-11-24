// Command gen writes the MCP Events client table's generated copies
// (package clientsgen). Run from the repository root (`make
// mcp-events-clients`) or internal/mcpevents/clientsgen (go generate).
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/wakeshift/server/internal/mcpevents/clientsgen"
)

func main() {
	root := "."
	if _, err := os.Stat("go.mod"); err != nil {
		root = "../../.."
	}
	files, err := clientsgen.Files(root)
	if err != nil {
		fmt.Fprintln(os.Stderr, "clientsgen:", err)
		os.Exit(1)
	}
	paths := make([]string, 0, len(files))
	for p := range files {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		if err := os.WriteFile(filepath.Join(root, p), files[p], 0o644); err != nil {
			fmt.Fprintln(os.Stderr, "clientsgen:", err)
			os.Exit(1)
		}
		fmt.Println("wrote", p)
	}
}
