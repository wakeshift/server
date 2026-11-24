package clientsgen

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// TestCopiesMatch fails when a copy of the client table isn't what
// sdk/typescript/test/mcp-events-clients.json makes now: run `make
// mcp-events-clients` and commit the result.
func TestCopiesMatch(t *testing.T) {
	root := filepath.Join("..", "..", "..")
	files, err := Files(root)
	if err != nil {
		t.Fatal(err)
	}
	for p, want := range files {
		got, err := os.ReadFile(filepath.Join(root, p))
		if err != nil {
			t.Errorf("%s: %v (run make mcp-events-clients)", p, err)
			continue
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%s is out of date: run make mcp-events-clients", p)
		}
	}
}
