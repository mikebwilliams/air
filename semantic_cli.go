package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"
)

func runSemanticCLI(ctx context.Context, args []string, environment cliEnvironment) error {
	command := args[0]
	flags := newFlagSet("inventory "+command, environment.Stderr)
	repo := flags.String("repo", ".", "scan checkout")
	asJSON := flags.Bool("json", false, "output JSON")
	selection := inventorySelection{Path: ".", Status: "included"}
	flags.StringVar(&selection.Path, "path", ".", "repository-relative prefix")
	flags.StringVar(&selection.Group, "group", "", "group filter")
	flags.StringVar(&selection.Tag, "tag", "", "tag filter")
	flags.StringVar(&selection.Status, "status", "included", "included, excluded, all, missing-command, header-unmapped")
	indexID := ""
	options := semanticIndexOptions{Clangd: "clangd", Jobs: 2, Timeout: 2 * time.Minute}
	if command == "index" {
		flags.StringVar(&options.Clangd, "clangd", "clangd", "clangd executable (21+)")
		flags.IntVar(&options.Jobs, "jobs", 2, "concurrent files in one shared clangd process")
		flags.DurationVar(&options.Timeout, "timeout", 2*time.Minute, "initialization and per-file timeout")
		flags.IntVar(&options.MaxFiles, "max-files", 0, "maximum files to attempt; 0 is unlimited")
		flags.BoolVar(&options.RetryErrors, "retry-errors", false, "retry partial, failed, and unavailable results")
		flags.BoolVar(&options.Refresh, "refresh", false, "reparse selected files, retaining older results")
	} else {
		flags.StringVar(&indexID, "index", "", "semantic profile ID; defaults to latest matching snapshot/build")
	}
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if flags.NArg() > 1 {
		return errors.New("accepts at most one inventory ID")
	}
	if err := selection.validate(); err != nil {
		return err
	}
	repository, err := DiscoverGitRepository(ctx, reposeAbsolutePath(environment.Cwd, *repo))
	if err != nil {
		return err
	}
	database, err := reposeDatabasePath(ctx, repository)
	if err != nil {
		return err
	}
	var store *inventoryStore
	if command == "index" {
		store, err = openInventoryStore(ctx, database, false)
	} else {
		store, err = openInventoryReadOnly(ctx, database)
	}
	if err != nil {
		return err
	}
	defer store.Close()
	record, err := store.Inventory(ctx, flags.Arg(0))
	if err != nil {
		return err
	}
	var snapshot *semanticSnapshot
	if command == "index" {
		lock, err := acquireScanLock(filepath.Join(filepath.Dir(database), "index.lock"))
		if err != nil {
			return fmt.Errorf("cannot acquire inventory index lock: %s", strings.ReplaceAll(err.Error(), "air scan", "inventory index"))
		}
		defer lock.Close()
		progress := environment.Stderr
		if progress == nil {
			progress = io.Discard
		}
		snapshot, err = runSemanticIndex(ctx, store, repository, record, selection, options, progress, func() time.Time { return environmentNow(environment) })
	} else {
		snapshot, err = store.semanticSnapshot(ctx, record.Inventory, indexID)
	}
	if err != nil {
		return err
	}
	if snapshot == nil {
		return errors.New("no semantic index for this snapshot/build; run repose inventory index")
	}
	selected := map[string]bool{}
	for _, file := range selection.files(record.Inventory) {
		if file.Kind == "source" || file.Kind == "header" {
			selected[file.Path] = true
		}
	}
	filtered := semanticSnapshot{Profile: snapshot.Profile, Files: []semanticFileResult{}}
	for _, file := range snapshot.Files {
		if selected[file.Path] {
			filtered.Files = append(filtered.Files, file)
		}
	}
	if *asJSON {
		return writeInventoryJSON(environment.Stdout, filtered)
	}
	fmt.Fprintf(environment.Stdout, "Index: %s  inventory: %s  snapshot: %s\n", snapshot.Profile.ID[:12], record.ID[:12], shortSHA(record.Inventory.SnapshotSHA))
	fmt.Fprintln(environment.Stdout, "Saved semantic facts; one compile variant per source. Header commands are borrowed from observed includers.")
	if command == "symbols" {
		writer := tabwriter.NewWriter(environment.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(writer, "FILE\tLINE\tKIND\tSYMBOL\tSTATUS")
		for _, file := range filtered.Files {
			printSemanticSymbols(writer, file.Path, file.Status, file.Symbols, "")
		}
		return writer.Flush()
	}
	if command == "includes" {
		writer := tabwriter.NewWriter(environment.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(writer, "FILE\tLINE\tRESOLVED INCLUDE\tSTATUS")
		for _, file := range filtered.Files {
			for _, include := range file.Includes {
				fmt.Fprintf(writer, "%q\t%d\t%q\t%s\n", file.Path, include.Range.Start.Line+1, include.Path, file.Status)
			}
		}
		return writer.Flush()
	}
	counts := map[string]int{}
	symbols, includes := 0, 0
	for _, file := range filtered.Files {
		counts[file.Status]++
		symbols += semanticSymbolCount(file.Symbols)
		includes += len(file.Includes)
	}
	fmt.Fprintf(environment.Stdout, "Selected: %d files; indexed: %d; partial: %d; unavailable: %d; failed: %d; pending: %d\nSymbols: %d; resolved include links: %d\n", len(selected), counts["indexed"], counts["partial"], counts["unavailable"], counts["failed"], len(selected)-len(filtered.Files), symbols, includes)
	writer := tabwriter.NewWriter(environment.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(writer, "FILE\tSTATUS\tCOMMAND\tSOURCE CONTEXT\tSYMBOLS\tDIAGNOSTICS")
	for _, file := range filtered.Files {
		fmt.Fprintf(writer, "%q\t%s\t%d\t%q\t%d\t%d\n", file.Path, file.Status, file.CommandID, file.CommandSource, semanticSymbolCount(file.Symbols), len(file.Diagnostics))
		if file.Error != "" {
			fmt.Fprintf(writer, "  %q\n", file.Error)
		}
		for _, diagnostic := range file.Diagnostics {
			if diagnostic.Severity == 1 {
				fmt.Fprintf(writer, "  line %d: %q\n", diagnostic.Range.Start.Line+1, diagnostic.Message)
			}
		}
	}
	return writer.Flush()
}

func printSemanticSymbols(output io.Writer, filename, status string, symbols []semanticSymbol, parent string) {
	for _, symbol := range symbols {
		name := symbol.Name
		if parent != "" {
			name = parent + "::" + name
		}
		fmt.Fprintf(output, "%q\t%d\t%s\t%q\t%s\n", filename, symbol.Range.Start.Line+1, semanticSymbolKind(symbol.Kind), name, status)
		printSemanticSymbols(output, filename, status, symbol.Children, name)
	}
}
func semanticSymbolKind(kind int) string {
	if name, ok := map[int]string{3: "namespace", 5: "class", 6: "method", 7: "property", 8: "field", 9: "constructor", 10: "enum", 11: "interface", 12: "function", 13: "variable", 14: "constant", 22: "enum-member", 23: "struct", 25: "operator", 26: "type-parameter"}[kind]; ok {
		return name
	}
	return fmt.Sprintf("kind-%d", kind)
}
