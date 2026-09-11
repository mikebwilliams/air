package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
)

func runInventoryPolicyCLI(ctx context.Context, args []string, environment cliEnvironment) error {
	command := args[0]
	if command == "policy" {
		if len(args) < 2 || (args[1] != "export" && args[1] != "import") {
			return errors.New("usage: repose inventory policy export [ID] | import FILE")
		}
		command = "policy " + args[1]
		args = args[1:]
	}
	flags := newFlagSet("inventory "+command, environment.Stderr)
	repoPath := flags.String("repo", ".", "dedicated scan checkout")
	asJSON := flags.Bool("json", false, "output JSON")
	var reason string
	editingScope := command == "exclude" || command == "include"
	if editingScope {
		flags.StringVar(&reason, "reason", "", "scope decision reason (required for exclusion)")
	}
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if editingScope {
		if flags.NArg() == 0 {
			return fmt.Errorf("usage: repose inventory %s PREFIX... [--reason TEXT]", command)
		}
		exclude := command == "exclude"
		for _, prefix := range flags.Args() {
			if err := validateInventoryPolicy(InventoryPolicy{Rules: []InventoryRule{{Prefix: prefix, Exclude: &exclude, Reason: reason}}}); err != nil {
				return err
			}
		}
	} else if command == "policy import" && flags.NArg() != 1 || command != "policy import" && flags.NArg() > 1 {
		return fmt.Errorf("unexpected arguments for repose inventory %s", command)
	}
	var policy InventoryPolicy
	if command == "policy import" {
		data, err := os.ReadFile(reposeAbsolutePath(environment.Cwd, flags.Arg(0)))
		if err != nil {
			return fmt.Errorf("read inventory policy: %w", err)
		}
		policy, err = readInventoryPolicy(strings.NewReader(string(data)))
		if err != nil {
			return err
		}
	}
	repository, err := DiscoverGitRepository(ctx, reposeAbsolutePath(environment.Cwd, *repoPath))
	if err != nil {
		return err
	}
	databasePath, err := reposeDatabasePath(ctx, repository)
	if err != nil {
		return err
	}
	var store *inventoryStore
	if editingScope || command == "policy import" {
		store, err = openInventoryStore(ctx, databasePath, false)
	} else {
		store, err = openInventoryReadOnly(ctx, databasePath)
	}
	if err != nil {
		return err
	}
	defer store.Close()
	selector := "current"
	if !editingScope && command != "policy import" && flags.NArg() == 1 {
		selector = flags.Arg(0)
	}
	record, err := store.Inventory(ctx, selector)
	if err != nil {
		return err
	}
	if command == "policy export" {
		return writeInventoryJSON(environment.Stdout, record.Inventory.Policy)
	}
	if command == "exclusions" {
		rows := inventoryExclusionRules(record.Inventory)
		if *asJSON {
			return writeInventoryJSON(environment.Stdout, struct {
				InventoryID string                   `json:"inventory_id"`
				Rules       []InventoryExclusionRule `json:"rules"`
			}{record.ID, rows})
		}
		fmt.Fprintf(environment.Stdout, "Inventory: %s\n", record.ID[:12])
		fmt.Fprintln(environment.Stdout, "Matched: C/C++ files under each prefix. Effective: files whose final scope is set by that rule.")
		fmt.Fprintln(environment.Stdout, "Other languages, symlinks and submodules remain outside review scope. Excluded code remains available as context.")
		writer := tabwriter.NewWriter(environment.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(writer, "SOURCE\tPREFIX\tACTION\tMATCHED\tEFFECTIVE\tREASON")
		for _, row := range rows {
			fmt.Fprintf(writer, "%s\t%q\t%s\t%d\t%d\t%q\n", row.Source, row.Prefix, row.Action, row.Matched, row.Effective, row.Reason)
		}
		return writer.Flush()
	}
	var updated Inventory
	var changes []InventoryScopeChange
	if editingScope {
		updated, changes, err = editInventoryScope(record.Inventory, flags.Args(), command == "exclude", reason)
	} else {
		updated, err = inventoryWithPolicy(record.Inventory, policy)
		if err == nil {
			if unmatched := inventoryUnmatchedPrefixes(updated); len(unmatched) > 0 {
				return fmt.Errorf("inventory policy prefix %q matches no tracked paths", unmatched[0])
			}
			changes = inventoryScopeChanges(record.Inventory, updated)
		}
	}
	if err != nil {
		return err
	}
	saved, err := store.saveCurrentInventory(ctx, updated, &record.ID, environmentNow(environment))
	if err != nil {
		return err
	}
	result := InventoryPolicyResult{PreviousID: record.ID, Record: saved, Changes: changes}
	if *asJSON {
		return writeInventoryJSON(environment.Stdout, result)
	}
	return printInventoryPolicyResult(environment.Stdout, result)
}
