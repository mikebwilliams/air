package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type semanticIndexOptions struct {
	Clangd               string
	Jobs                 int
	Timeout              time.Duration
	MaxFiles             int
	RetryErrors, Refresh bool
}

func runSemanticIndex(ctx context.Context, store *inventoryStore, repository *GitRepository, record InventoryRecord, selection inventorySelection, options semanticIndexOptions, progress io.Writer, now func() time.Time) (*semanticSnapshot, error) {
	if options.Jobs < 1 || options.Jobs > 32 {
		return nil, errors.New("--jobs must be between 1 and 32")
	}
	if options.Timeout <= 0 || options.MaxFiles < 0 {
		return nil, errors.New("--timeout must be positive and --max-files nonnegative")
	}
	if err := checkInventory(ctx, repository, record.Inventory); err != nil {
		return nil, err
	}
	profile, err := makeSemanticProfile(ctx, record.Inventory, options.Clangd)
	if err != nil {
		return nil, err
	}
	if err := store.saveSemanticProfile(ctx, profile, now()); err != nil {
		return nil, err
	}
	snapshot, err := store.semanticSnapshot(ctx, record.Inventory, profile.ID)
	if err != nil {
		return nil, err
	}
	results := snapshot.byPath()
	files := selection.files(record.Inventory)
	selected := []InventoryFile{}
	for _, file := range files {
		if file.Kind == "source" || file.Kind == "header" {
			selected = append(selected, file)
		}
	}
	if len(selected) == 0 {
		return nil, errors.New("selection contains no C/C++ files to index")
	}
	// Use a private compilation database; source files/build configuration stay
	// untouched. The first saved variant for each input is the declared policy.
	directory, err := os.MkdirTemp("", "repose-clangd-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(directory)
	commands := []InventoryCommand{}
	seen := map[string]bool{}
	for _, command := range record.Inventory.Commands {
		if !seen[command.File] {
			commands = append(commands, command)
			seen[command.File] = true
		}
	}
	data, err := json.Marshal(commands)
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(directory, "compile_commands.json"), data, 0o600); err != nil {
		return nil, err
	}
	runContext, cancel := context.WithCancel(ctx)
	defer cancel()
	// The process has the full run lifetime; only a failed initialization
	// deadline cancels it. Successful initialization stops the timer.
	var client *clangdClient
	initializationTimer := time.AfterFunc(options.Timeout, cancel)
	client, err = startClangd(runContext, profile.Clangd, directory, record.Inventory.WorkTree, options.Jobs)
	initializationTimer.Stop()
	if err != nil {
		return nil, err
	}
	defer client.Close()
	attempted := map[string]bool{}
	remaining := options.MaxFiles
	shouldIndex := func(file InventoryFile) bool {
		if attempted[file.Path] {
			return false
		}
		previous, exists := results[file.Path]
		return !exists || options.Refresh || semanticDependenciesChanged(previous) || options.RetryErrors && previous.Status != "indexed"
	}
	// Each batch saves results as workers finish. Interrupted requests aren't
	// published; the next invocation resumes from the completed per-file records.
	process := func(batch []InventoryFile, contexts map[string]int) error {
		if options.MaxFiles > 0 && len(batch) > remaining {
			batch = batch[:remaining]
		}
		if len(batch) == 0 {
			return nil
		}
		for _, file := range batch {
			attempted[file.Path] = true
		}
		remaining -= len(batch)
		jobs := make(chan InventoryFile)
		completed := make(chan semanticFileResult, options.Jobs)
		var workers sync.WaitGroup
		for i := 0; i < min(options.Jobs, len(batch)); i++ {
			workers.Add(1)
			go func() {
				defer workers.Done()
				for file := range jobs {
					result := indexSemanticFile(runContext, client, profile, record.Inventory, file, contexts[file.Path], options.Timeout)
					if runContext.Err() != nil {
						return
					}
					select {
					case completed <- result:
					case <-runContext.Done():
						return
					}
				}
			}()
		}
		go func() {
			defer close(jobs)
			for _, file := range batch {
				select {
				case jobs <- file:
				case <-runContext.Done():
					return
				}
			}
		}()
		go func() { workers.Wait(); close(completed) }()
		var failure error
		for result := range completed {
			if failure != nil {
				continue
			}
			saved, err := store.saveSemanticResult(ctx, result, now())
			if err != nil {
				failure = err
				cancel()
				continue
			}
			results[saved.Path] = saved
			fmt.Fprintf(progress, "%s  %s  %d symbols, %d includes, %d diagnostics\n", saved.Status, saved.Path, semanticSymbolCount(saved.Symbols), len(saved.Includes), len(saved.Diagnostics))
			if saved.Status == "failed" {
				failure = fmt.Errorf("index %s: %s (completed files are saved; use --retry-errors to retry)", saved.Path, saved.Error)
				cancel()
			}
		}
		if failure != nil {
			return failure
		}
		return runContext.Err()
	}
	finish := func() (*semanticSnapshot, error) {
		if err := checkInventory(ctx, repository, record.Inventory); err != nil {
			return nil, err
		}
		return &semanticSnapshot{Profile: profile, Files: semanticSortedResults(results)}, nil
	}
	contexts := map[string]int{}
	sources := []InventoryFile{}
	for _, file := range selected {
		if file.Kind == "source" && shouldIndex(file) {
			sources = append(sources, file)
			if len(file.CommandIDs) > 0 {
				contexts[file.Path] = file.CommandIDs[0]
			}
		}
	}
	if err := process(sources, contexts); err != nil {
		return nil, err
	}
	for {
		if options.MaxFiles > 0 && remaining == 0 {
			return finish()
		}
		contexts = semanticHeaderContexts(results)
		headers := []InventoryFile{}
		for _, file := range selected {
			newContext := results[file.Path].Status == "unavailable" && !attempted[file.Path]
			if file.Kind == "header" && (shouldIndex(file) || newContext) && contexts[file.Path] > 0 {
				headers = append(headers, file)
			}
		}
		if len(headers) == 0 {
			break
		}
		if err := process(headers, contexts); err != nil {
			return nil, err
		}
	}
	// Remaining headers have no observed includer in the material indexed so
	// far. Retain the gap; a later broader index can supply their context.
	headers := []InventoryFile{}
	for _, file := range selected {
		if file.Kind == "header" && shouldIndex(file) {
			headers = append(headers, file)
		}
	}
	if err := process(headers, map[string]int{}); err != nil {
		return nil, err
	}
	return finish()
}

func semanticHeaderContexts(results map[string]semanticFileResult) map[string]int {
	contexts := map[string]int{}
	// Existing header contexts remain stable across resume/subset expansion.
	for _, file := range semanticSortedResults(results) {
		if file.CommandID > 0 && file.Status != "failed" && file.Status != "unavailable" {
			for _, link := range file.Includes {
				if link.Path != "" && !filepath.IsAbs(link.Path) && (contexts[link.Path] == 0 || file.CommandID < contexts[link.Path]) {
					contexts[link.Path] = file.CommandID
				}
			}
		}
	}
	for _, file := range results {
		if file.CommandID > 0 && (file.Status == "indexed" || file.Status == "partial") {
			contexts[file.Path] = file.CommandID
		}
	}
	return contexts
}

func indexSemanticFile(ctx context.Context, client *clangdClient, profile semanticProfile, inventory Inventory, file InventoryFile, commandID int, timeout time.Duration) semanticFileResult {
	result := semanticFileResult{ProfileID: profile.ID, Path: file.Path, BlobID: file.BlobID, Bytes: file.Bytes, CommandID: commandID, Status: "unavailable", Symbols: []semanticSymbol{}, Includes: []semanticLink{}, Diagnostics: []semanticDiagnostic{}}
	if commandID < 1 || commandID > len(inventory.Commands) {
		result.Error = "no saved compilation command"
		if file.Kind == "header" {
			result.Error = "no observed source includer; index its source files first"
		}
		return result
	}
	command := inventory.Commands[commandID-1]
	relative, _ := filepath.Rel(inventory.WorkTree, command.File)
	result.CommandSource = filepath.ToSlash(relative)
	var err error
	if file.Kind == "header" {
		command, err = semanticHeaderCommand(command, filepath.Join(inventory.WorkTree, filepath.FromSlash(file.Path)))
	} else {
		command.Arguments, err = semanticCommandArguments(command)
		command.Command = ""
	}
	if err != nil {
		result.Status = "failed"
		result.Error = err.Error()
		return result
	}
	result.CommandArguments = command.Arguments
	contents, err := semanticSourceText(inventory, file)
	if err != nil {
		result.Status = "failed"
		result.Error = err.Error()
		return result
	}
	parseContext, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	result.Symbols, result.Includes, result.Diagnostics, err = client.parse(parseContext, filepath.Join(inventory.WorkTree, filepath.FromSlash(file.Path)), file.Kind, contents, command)
	if err == nil {
		err = semanticSymbolOffsets(result.Symbols, contents)
	}
	if err != nil {
		result.Status = "failed"
		result.Error = err.Error()
		return result
	}
	result.Includes = semanticResolvedLinks(inventory.WorkTree, result.Includes)
	result.Status = "indexed"
	result.BuildInputs, err = semanticForcedInputs(command)
	if err != nil {
		result.Status = "partial"
		result.Error = err.Error()
	}
	for _, diagnostic := range result.Diagnostics {
		if diagnostic.Severity == 1 {
			result.Status = "partial"
		}
	}
	for _, link := range result.Includes {
		if link.Error != "" {
			result.Status = "partial"
		}
	}
	return result
}

func semanticForcedInputs(command InventoryCommand) ([]InventoryBuildFile, error) {
	inputs := []InventoryBuildFile{}
	for index, arg := range command.Arguments {
		if arg != "-include" && arg != "-imacros" && arg != "-include-pch" {
			continue
		}
		if index+1 >= len(command.Arguments) {
			return inputs, fmt.Errorf("missing argument for %s", arg)
		}
		filename := command.Arguments[index+1]
		if !filepath.IsAbs(filename) {
			filename = filepath.Join(command.Directory, filename)
		}
		data, err := os.ReadFile(filename)
		if err != nil {
			return inputs, fmt.Errorf("fingerprint forced include %s: %w", filename, err)
		}
		inputs = append(inputs, InventoryBuildFile{Path: filepath.Clean(filename), SHA256: inventoryHash(data)})
	}
	return inputs, nil
}

func semanticDependenciesChanged(result semanticFileResult) bool {
	inputs := append([]InventoryBuildFile{}, result.BuildInputs...)
	for _, include := range result.Includes {
		if include.SHA256 == "" {
			continue
		}
		filename, err := clangdPath(include.Target)
		if err != nil {
			return true
		}
		inputs = append(inputs, InventoryBuildFile{Path: filename, SHA256: include.SHA256})
	}
	for _, input := range inputs {
		data, err := os.ReadFile(input.Path)
		if err != nil || inventoryHash(data) != input.SHA256 {
			return true
		}
	}
	return false
}
