package cli

import (
	"bytes"
	"testing"
)

func TestCommandTree(t *testing.T) {
	root := NewRootCmd()
	for _, path := range [][]string{
		{"source", "add"}, {"source", "ls"},
		{"branch", "create"}, {"branch", "ls"}, {"branch", "destroy"},
		{"connect"},
	} {
		cmd, _, err := root.Find(path)
		if err != nil || cmd.Name() != path[len(path)-1] {
			t.Fatalf("command %v not found: %v", path, err)
		}
	}
	// help renders without side effects
	var buf bytes.Buffer
	root.SetOut(&buf)
	root.SetArgs([]string{"--help"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
}
