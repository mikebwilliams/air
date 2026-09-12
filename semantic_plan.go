package main

import (
	"encoding/json"
	"fmt"
	"path"
	"path/filepath"
	"slices"
	"sort"
)

type inventoryTargetRange struct {
	Path      string   `json:"path"`
	BlobID    string   `json:"blob_id"`
	StartByte int64    `json:"start_byte"`
	EndByte   int64    `json:"end_byte"`
	Symbols   []string `json:"symbols,omitempty"`
}
type inventoryContextFile struct {
	Path   string `json:"path"`
	BlobID string `json:"blob_id,omitempty"`
	SHA256 string `json:"sha256,omitempty"`
	Reason string `json:"reason"`
}
type inventorySemanticResultRef struct {
	Path string `json:"path"`
	ID   string `json:"id"`
}

func planSemanticInventory(record InventoryRecord, selection inventorySelection, goal string, limits inventoryPlanLimits, snapshot *semanticSnapshot) (inventoryPlan, error) {
	plan, err := planInventory(record, selection, goal, limits)
	if err != nil {
		return inventoryPlan{}, err
	}
	if snapshot == nil {
		return inventoryPlan{}, fmt.Errorf("no semantic index for this snapshot/build; run inventory index")
	}
	if snapshot.Profile.BaseID != semanticBaseID(record.Inventory) {
		return inventoryPlan{}, fmt.Errorf("semantic index belongs to a different snapshot/build")
	}
	// Reuse the file planner's selection validation and ordering, then repartition
	// targets using persisted symbol boundaries. Source bytes need not be read.
	files := []InventoryFile{}
	for _, assignment := range plan.Assignments {
		files = append(files, assignment.Files...)
	}
	plan.ID = ""
	plan.Planner = "symbols-v1"
	plan.Assignments = []inventoryAssignment{}
	plan.SemanticProfileID = snapshot.Profile.ID
	plan.SemanticResults = []inventorySemanticResultRef{}
	indexed := snapshot.byPath()
	tracked := map[string]InventoryFile{}
	for _, file := range record.Inventory.Files {
		tracked[file.Path] = file
	}
	var current *inventoryAssignment
	for _, file := range files {
		semantic, exists := indexed[file.Path]
		if exists && semantic.BlobID != file.BlobID {
			return inventoryPlan{}, fmt.Errorf("semantic result for %s has a different blob", file.Path)
		}
		warnings := []string{}
		ranges := []inventoryTargetRange{{Path: file.Path, BlobID: file.BlobID, EndByte: file.Bytes}}
		if exists {
			plan.SemanticResults = append(plan.SemanticResults, inventorySemanticResultRef{file.Path, semantic.ID})
			if semantic.Status == "indexed" {
				ranges, err = semanticTargetRanges(file, semantic.Symbols, limits.MaxBytes)
				if err != nil {
					return inventoryPlan{}, err
				}
			} else {
				warnings = append(warnings, "semantic status "+semantic.Status+": whole file retained")
			}
		} else {
			warnings = append(warnings, "file has not been indexed; whole file retained")
		}
		if file.Kind == "source" && len(file.CommandIDs) == 0 {
			warnings = append(warnings, "no compilation command")
		}
		if file.Kind == "source" && len(file.CommandIDs) > 1 {
			warnings = append(warnings, "only the first saved compile variant was indexed")
		}
		context := []inventoryContextFile{}
		if exists {
			for _, include := range semantic.Includes {
				if include.Path == "" || filepath.IsAbs(include.Path) {
					continue
				}
				context = append(context, inventoryContextFile{Path: include.Path, BlobID: tracked[include.Path].BlobID, SHA256: include.SHA256, Reason: "resolved include from " + file.Path})
			}
			if file.Kind == "header" && semantic.CommandSource != "" {
				context = append(context, inventoryContextFile{Path: semantic.CommandSource, BlobID: tracked[semantic.CommandSource].BlobID, Reason: "source command borrowed for header parsing"})
			}
		}
		for _, part := range ranges {
			bytes := part.EndByte - part.StartByte
			hasFile := current != nil && slices.ContainsFunc(current.Files, func(candidate InventoryFile) bool { return candidate.Path == file.Path })
			if current == nil || current.Group != file.Group || current.Directory != path.Dir(file.Path) || !hasFile && len(current.Files) >= limits.MaxFiles || bytes > limits.MaxBytes-current.Bytes {
				plan.Assignments = append(plan.Assignments, inventoryAssignment{Group: file.Group, Directory: path.Dir(file.Path), Files: []InventoryFile{}, Warnings: []string{}, Ranges: []inventoryTargetRange{}, Context: []inventoryContextFile{}})
				current = &plan.Assignments[len(plan.Assignments)-1]
				hasFile = false
			}
			if !hasFile {
				current.Files = append(current.Files, file)
			}
			if len(current.Ranges) > 0 && current.Ranges[len(current.Ranges)-1].Path == part.Path && current.Ranges[len(current.Ranges)-1].EndByte == part.StartByte {
				previous := &current.Ranges[len(current.Ranges)-1]
				previous.EndByte = part.EndByte
				previous.Symbols = inventoryUniqueStrings(previous.Symbols, part.Symbols...)
			} else {
				current.Ranges = append(current.Ranges, part)
			}
			current.Bytes += bytes
			current.Warnings = inventoryUniqueStrings(current.Warnings, warnings...)
			if bytes > limits.MaxBytes {
				current.Oversized = true
				current.Warnings = inventoryUniqueStrings(current.Warnings, "unsplittable target range exceeds byte limit; explicit size decision required")
			}
			for _, dependency := range context {
				if !slices.ContainsFunc(current.Context, func(previous inventoryContextFile) bool {
					return previous.Path == dependency.Path && previous.Reason == dependency.Reason
				}) {
					current.Context = append(current.Context, dependency)
				}
			}
		}
	}
	data, err := json.Marshal(plan)
	if err != nil {
		return inventoryPlan{}, err
	}
	plan.ID = inventoryHash(data)
	for i := range plan.Assignments {
		plan.Assignments[i].ID = inventoryHash([]byte(fmt.Sprintf("%s:%d", plan.ID, i)))
	}
	return plan, nil
}

func semanticTargetRanges(file InventoryFile, symbols []semanticSymbol, limit int64) ([]inventoryTargetRange, error) {
	type atom struct {
		start, end int64
		names      []string
	}
	atoms := []atom{}
	var visit func([]semanticSymbol, string) error
	visit = func(items []semanticSymbol, parent string) error {
		for _, symbol := range items {
			if symbol.StartByte < 0 || symbol.EndByte < symbol.StartByte || symbol.EndByte > file.Bytes {
				return fmt.Errorf("invalid saved symbol range in %s", file.Path)
			}
			name := symbol.Name
			if parent != "" {
				name = parent + "::" + name
			}
			container := symbol.Kind == 3 || symbol.Kind == 5 || symbol.Kind == 23
			if container && len(symbol.Children) > 0 && (symbol.Kind == 3 || symbol.EndByte-symbol.StartByte > limit) {
				if err := visit(symbol.Children, name); err != nil {
					return err
				}
				continue
			}
			if symbol.EndByte > symbol.StartByte {
				atoms = append(atoms, atom{symbol.StartByte, symbol.EndByte, []string{name}})
			}
		}
		return nil
	}
	if err := visit(symbols, ""); err != nil {
		return nil, err
	}
	sort.Slice(atoms, func(i, j int) bool {
		if atoms[i].start != atoms[j].start {
			return atoms[i].start < atoms[j].start
		}
		return atoms[i].end < atoms[j].end
	})
	merged := []atom{}
	for _, item := range atoms {
		if len(merged) > 0 && item.start < merged[len(merged)-1].end {
			previous := &merged[len(merged)-1]
			previous.end = max(previous.end, item.end)
			previous.names = inventoryUniqueStrings(previous.names, item.names...)
		} else {
			merged = append(merged, item)
		}
	}
	ranges := []inventoryTargetRange{}
	cursor := int64(0)
	appendRange := func(start, end int64, names []string) {
		if end > start {
			ranges = append(ranges, inventoryTargetRange{file.Path, file.BlobID, start, end, names})
		}
	}
	for _, item := range merged {
		appendRange(cursor, item.start, nil)
		appendRange(item.start, item.end, item.names)
		cursor = item.end
	}
	appendRange(cursor, file.Bytes, nil)
	if len(ranges) == 0 {
		ranges = append(ranges, inventoryTargetRange{Path: file.Path, BlobID: file.BlobID, EndByte: file.Bytes})
	}
	return ranges, nil
}
