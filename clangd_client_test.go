package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestClangdClientUsesExplicitCommandsAndDiagnostics(t *testing.T) {
	executable, err := exec.LookPath("clangd")
	if err != nil {
		t.Skip("clangd not installed")
	}
	directory := t.TempDir()
	source := filepath.Join(directory, "test +#.cpp")
	contents := []byte("#ifndef EXPECTED\n#error wrong compilation command\n#endif\n// π\nstruct Widget { int value; };\nint run() { return EXPECTED; }\n")
	if err := os.WriteFile(source, contents, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "compile_commands.json"), []byte("[]"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	client, err := startClangd(ctx, executable, directory, directory, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	symbols, _, diagnostics, err := client.parse(ctx, source, "source", contents, InventoryCommand{Directory: directory, File: source, Arguments: []string{"clang++", "-DEXPECTED=1", "-c", source}})
	if err != nil {
		t.Fatalf("parse: %v\n%s", err, client.log.String())
	}
	for _, diagnostic := range diagnostics {
		if diagnostic.Severity == 1 {
			t.Fatalf("wrong compile command: %+v\n%s", diagnostic, client.log.String())
		}
	}
	if semanticSymbolCount(symbols) < 3 {
		t.Fatalf("missing symbols: %+v", symbols)
	}
	if err := semanticSymbolOffsets(symbols, contents); err != nil {
		t.Fatal(err)
	}
	for _, symbol := range symbols {
		if symbol.Name == "run" && !strings.HasPrefix(string(contents[symbol.StartByte:symbol.EndByte]), "int run") {
			t.Fatal("incorrect byte range")
		}
	}
}
