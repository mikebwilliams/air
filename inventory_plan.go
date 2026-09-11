package main

import (
	"encoding/json"
	"fmt"
	"path"
	"sort"
	"strings"
)

const inventoryPlannerVersion = "files-v1"

type inventoryPlanLimits struct {
	MaxFiles int   `json:"max_files"`
	MaxBytes int64 `json:"max_bytes"`
}

type inventoryAssignment struct {
	ID        string          `json:"id"`
	Group     string          `json:"group"`
	Directory string          `json:"directory"`
	Bytes     int64           `json:"bytes"`
	Oversized bool            `json:"oversized"`
	Files     []InventoryFile `json:"files"`
	Warnings  []string        `json:"warnings"`
}

type inventoryPlan struct {
	ID          string                `json:"id"`
	Planner     string                `json:"planner"`
	InventoryID string                `json:"inventory_id"`
	SnapshotSHA string                `json:"snapshot_sha"`
	Goal        string                `json:"goal"`
	Selection   inventorySelection    `json:"selection"`
	Limits      inventoryPlanLimits   `json:"limits"`
	Files       int                   `json:"files"`
	Bytes       int64                 `json:"bytes"`
	Assignments []inventoryAssignment `json:"assignments"`
}

// This preview partitions targets, not semantic review units. Sorted files are
// packed within a group/directory. Each selected file occurs exactly once;
// oversized files remain visible as singleton assignments, never truncated.
func planInventory(record InventoryRecord, selection inventorySelection, goal string, limits inventoryPlanLimits) (inventoryPlan, error) {
	if err := selection.validate(); err != nil {
		return inventoryPlan{}, err
	}
	if selection.Status != "included" {
		return inventoryPlan{}, fmt.Errorf("assignment previews require included scope")
	}
	if strings.TrimSpace(goal) == "" {
		return inventoryPlan{}, fmt.Errorf("goal must not be blank")
	}
	if limits.MaxFiles < 1 || limits.MaxBytes < 1 {
		return inventoryPlan{}, fmt.Errorf("--max-files and --max-bytes must be positive")
	}
	files := selection.files(record.Inventory)
	if len(files) == 0 {
		return inventoryPlan{}, fmt.Errorf("selection contains no included C/C++ files")
	}
	sort.Slice(files, func(i, j int) bool {
		if files[i].Group != files[j].Group {
			return files[i].Group < files[j].Group
		}
		if path.Dir(files[i].Path) != path.Dir(files[j].Path) {
			return path.Dir(files[i].Path) < path.Dir(files[j].Path)
		}
		return files[i].Path < files[j].Path
	})
	plan := inventoryPlan{Planner: inventoryPlannerVersion, InventoryID: record.ID, SnapshotSHA: record.Inventory.SnapshotSHA,
		Goal: goal, Selection: selection, Limits: limits, Files: len(files), Assignments: []inventoryAssignment{}}
	var current *inventoryAssignment
	for _, file := range files {
		directory := path.Dir(file.Path)
		if current == nil || current.Group != file.Group || current.Directory != directory ||
			len(current.Files) >= limits.MaxFiles || file.Bytes > limits.MaxBytes-current.Bytes {
			plan.Assignments = append(plan.Assignments, inventoryAssignment{Group: file.Group, Directory: directory,
				Files: []InventoryFile{}, Warnings: []string{}})
			current = &plan.Assignments[len(plan.Assignments)-1]
		}
		current.Files = append(current.Files, file)
		current.Bytes += file.Bytes
		plan.Bytes += file.Bytes
		if file.Bytes > limits.MaxBytes {
			current.Oversized = true
			current.Warnings = append(current.Warnings, "file exceeds byte limit; requires splitting or an explicit size decision")
		}
		if file.Kind == "source" && len(file.CommandIDs) == 0 {
			current.Warnings = append(current.Warnings, fmt.Sprintf("no compilation command: %s", file.Path))
		}
		if file.Kind == "header" {
			current.Warnings = inventoryUniqueStrings(current.Warnings, "headers await semantic association")
		}
	}
	// IDs depend only on frozen inputs and the planner version, not timestamps
	// or review acknowledgement. They are preview identities, not queue records.
	data, err := json.Marshal(plan)
	if err != nil {
		return inventoryPlan{}, err
	}
	plan.ID = inventoryHash(data)
	for index := range plan.Assignments {
		plan.Assignments[index].ID = inventoryHash([]byte(fmt.Sprintf("%s:%d", plan.ID, index)))
	}
	return plan, nil
}
