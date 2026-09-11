package main

import (
	"fmt"
	"path"
	"slices"
	"sort"
	"strings"
)

type inventorySelection struct {
	Path   string `json:"path"`
	Group  string `json:"group,omitempty"`
	Tag    string `json:"tag,omitempty"`
	Status string `json:"status"`
}

func (selection inventorySelection) validate() error {
	if err := validateInventoryPolicy(InventoryPolicy{Rules: []InventoryRule{{Prefix: selection.Path}}}); err != nil {
		return fmt.Errorf("invalid --path: %w", err)
	}
	switch selection.Status {
	case "included", "excluded", "all", "missing-command", "header-unmapped":
		return nil
	default:
		return fmt.Errorf("unknown file status %q", selection.Status)
	}
}

func (selection inventorySelection) files(inventory Inventory) []InventoryFile {
	files := []InventoryFile{}
	for _, file := range inventory.Files {
		if inventoryPrefixMatches(file.Path, selection.Path) &&
			(selection.Group == "" || selection.Group == file.Group) &&
			(selection.Tag == "" || slices.Contains(file.Tags, selection.Tag)) &&
			inventoryFileMatchesStatus(file, selection.Status) {
			files = append(files, file)
		}
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	return files
}

type inventoryCounts struct {
	Files          int   `json:"files"`
	Bytes          int64 `json:"bytes"`
	Sources        int   `json:"sources"`
	Headers        int   `json:"headers"`
	Excluded       int   `json:"excluded"`
	MissingCommand int   `json:"missing_command"`
	UnmappedHeader int   `json:"unmapped_header"`
}

func (counts *inventoryCounts) add(file InventoryFile) {
	counts.Files++
	counts.Bytes += file.Bytes
	if file.Kind == "source" {
		counts.Sources++
	}
	if file.Kind == "header" {
		counts.Headers++
	}
	if file.Excluded {
		counts.Excluded++
	}
	if inventoryFileMatchesStatus(file, "missing-command") {
		counts.MissingCommand++
	}
	if inventoryFileMatchesStatus(file, "header-unmapped") {
		counts.UnmappedHeader++
	}
}

type inventoryTreeRow struct {
	Path  string `json:"path"`
	Depth int    `json:"depth"`
	inventoryCounts
}

// Directory totals include all selected descendants, even below the display
// depth. A file prefix produces one row. No source or build access is needed.
func inventoryTree(files []InventoryFile, prefix string, depth int) []inventoryTreeRow {
	counts := map[string]*inventoryCounts{prefix: {}}
	for _, file := range files {
		counts[prefix].add(file)
		if file.Path == prefix {
			continue
		}
		for directory := path.Dir(file.Path); directory != prefix && inventoryPrefixMatches(directory, prefix); directory = path.Dir(directory) {
			if counts[directory] == nil {
				counts[directory] = &inventoryCounts{}
			}
			counts[directory].add(file)
			if directory == "." {
				break
			}
		}
	}
	rows := []inventoryTreeRow{}
	for directory, total := range counts {
		level := 0
		for parent := directory; parent != prefix; parent = path.Dir(parent) {
			level++
		}
		if level <= depth {
			rows = append(rows, inventoryTreeRow{directory, level, *total})
		}
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Path == rows[j].Path {
			return false
		}
		if rows[i].Path == prefix {
			return true
		}
		if rows[j].Path == prefix {
			return false
		}
		// Compare path components so siblings such as a.b cannot appear between
		// directory a and its descendants. The selected root is always first.
		return slices.Compare(strings.Split(rows[i].Path, "/"), strings.Split(rows[j].Path, "/")) < 0
	})
	return rows
}

type inventoryGroupSummary struct {
	Name string `json:"name"`
	inventoryCounts
	Tags  []string `json:"tags"`
	Notes []string `json:"notes"`
}

func inventoryGroups(files []InventoryFile) []inventoryGroupSummary {
	groups := map[string]*inventoryGroupSummary{}
	for _, file := range files {
		group := groups[file.Group]
		if group == nil {
			group = &inventoryGroupSummary{Name: file.Group, Tags: []string{}, Notes: []string{}}
			groups[file.Group] = group
		}
		group.add(file)
		group.Tags = inventoryUniqueStrings(group.Tags, file.Tags...)
		group.Notes = inventoryUniqueStrings(group.Notes, file.Notes...)
	}
	result := []inventoryGroupSummary{}
	for _, group := range groups {
		result = append(result, *group)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Name < result[j].Name })
	return result
}

func inventoryUniqueStrings(existing []string, values ...string) []string {
	for _, value := range values {
		if !slices.Contains(existing, value) {
			existing = append(existing, value)
		}
	}
	sort.Strings(existing)
	return existing
}

type inventoryInspection struct {
	InventoryID string             `json:"inventory_id"`
	Selection   inventorySelection `json:"selection"`
	inventoryCounts
	Groups  []inventoryGroupSummary `json:"groups"`
	Largest []InventoryFile         `json:"largest_files"`
	Rules   []InventoryRule         `json:"matching_policy_rules"`
}

func inspectInventory(record InventoryRecord, selection inventorySelection) inventoryInspection {
	files := selection.files(record.Inventory)
	result := inventoryInspection{InventoryID: record.ID, Selection: selection,
		Groups: inventoryGroups(files), Rules: []InventoryRule{}}
	for _, file := range files {
		result.add(file)
	}
	for _, rule := range append(inventoryDefaultExclusions(), record.Inventory.Policy.Rules...) {
		for _, file := range files {
			if inventoryPrefixMatches(file.Path, rule.Prefix) {
				result.Rules = append(result.Rules, rule)
				break
			}
		}
	}
	sort.Slice(files, func(i, j int) bool {
		if files[i].Bytes != files[j].Bytes {
			return files[i].Bytes > files[j].Bytes
		}
		return files[i].Path < files[j].Path
	})
	result.Largest = files[:min(10, len(files))]
	return result
}
