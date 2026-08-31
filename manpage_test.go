package main

import (
	"os"
	"strings"
	"testing"
)

func TestManPageCoversCommandCatalogAndEnvironment(t *testing.T) {
	contents, err := os.ReadFile("man/air.1")
	if err != nil {
		t.Fatal(err)
	}
	page := string(contents)
	normalizedPage := strings.ReplaceAll(page, `\-`, "-")

	var checkCommands func(prefix string, commands []cliCommandSpec)
	checkCommands = func(prefix string, commands []cliCommandSpec) {
		for _, command := range commands {
			path := strings.TrimSpace(prefix + " " + command.Name)
			if !strings.Contains(page, "air "+path) {
				t.Errorf("man page does not mention command %q", "air "+path)
			}
			for _, option := range command.Options {
				name := strings.Fields(option.Syntax)[0]
				if !strings.Contains(normalizedPage, name) {
					t.Errorf("man page does not mention option %q", name)
				}
			}
			checkCommands(path, command.Children)
		}
	}
	checkCommands("", cliCommands)

	for _, setting := range reviewSettings {
		for _, name := range setting.Environment {
			if !strings.Contains(page, name) {
				t.Errorf("man page does not mention environment variable %q", name)
			}
		}
	}
}
