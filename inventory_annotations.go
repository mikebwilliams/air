package main

import (
	"fmt"
	"slices"
	"strings"
)

type inventoryAnnotationEdit struct {
	Group string
	Tags  []string
	Note  string
}

// Group replaces the assignment for each prefix; tags and notes are additive.
// Clearing/removing inherited annotations is supported through policy import.
func editInventoryAnnotations(inventory Inventory, prefixes []string, edit inventoryAnnotationEdit) (Inventory, error) {
	if edit.Group == "" && len(edit.Tags) == 0 && edit.Note == "" {
		return Inventory{}, fmt.Errorf("provide --name, --tag, or --note")
	}
	if edit.Note != "" && strings.TrimSpace(edit.Note) == "" {
		return Inventory{}, fmt.Errorf("note must not be blank")
	}
	for _, tag := range edit.Tags {
		if strings.TrimSpace(tag) == "" {
			return Inventory{}, fmt.Errorf("tag must not be blank")
		}
	}
	policy := InventoryPolicy{Rules: slices.Clone(inventory.Policy.Rules)}
	for _, prefix := range prefixes {
		rule := InventoryRule{Prefix: prefix, Group: edit.Group, Tags: inventoryUniqueStrings([]string{}, edit.Tags...), Note: edit.Note}
		if err := validateInventoryPolicy(InventoryPolicy{Rules: []InventoryRule{rule}}); err != nil {
			return Inventory{}, err
		}
		matched := false
		for _, file := range inventory.Files {
			if inventoryPrefixMatches(file.Path, prefix) {
				matched = true
				break
			}
		}
		if !matched {
			return Inventory{}, fmt.Errorf("prefix %q matches no tracked inventory files", prefix)
		}
		// A repeated group command need not move its rule past unrelated notes
		// or tags. Preserve the version unless a later overlapping group wins.
		if rule.Group != "" {
			for index := len(policy.Rules) - 1; index >= 0; index-- {
				previous := policy.Rules[index]
				if previous.Group == "" || !(inventoryPrefixMatches(prefix, previous.Prefix) || inventoryPrefixMatches(previous.Prefix, prefix)) {
					continue
				}
				if previous.Prefix == prefix && previous.Group == rule.Group {
					rule.Group = ""
				}
				break
			}
		}
		rules := []InventoryRule{}
		for _, previous := range policy.Rules {
			if previous.Prefix == prefix {
				if rule.Group != "" {
					previous.Group = ""
				}
				if rule.Note != "" && previous.Note == rule.Note {
					rule.Note = ""
				}
				rule.Tags = slices.DeleteFunc(rule.Tags, func(tag string) bool { return slices.Contains(previous.Tags, tag) })
			}
			if previous.Group != "" || previous.Exclude != nil || len(previous.Tags) > 0 || previous.Note != "" {
				rules = append(rules, previous)
			}
		}
		if rule.Group != "" || len(rule.Tags) > 0 || rule.Note != "" {
			rules = append(rules, rule)
		}
		policy.Rules = rules
	}
	return inventoryWithPolicy(inventory, policy)
}

type inventoryAnnotationChange struct {
	Path   string                    `json:"path"`
	Before inventoryAnnotationValues `json:"before"`
	After  inventoryAnnotationValues `json:"after"`
}

type inventoryAnnotationValues struct {
	Group string   `json:"group"`
	Tags  []string `json:"tags"`
	Notes []string `json:"notes"`
}

func inventoryAnnotationChanges(before, after Inventory) []inventoryAnnotationChange {
	changes := []inventoryAnnotationChange{}
	for index, file := range after.Files {
		old := before.Files[index]
		if file.Group == old.Group && slices.Equal(file.Tags, old.Tags) && slices.Equal(file.Notes, old.Notes) {
			continue
		}
		changes = append(changes, inventoryAnnotationChange{file.Path,
			inventoryAnnotationValues{old.Group, old.Tags, old.Notes}, inventoryAnnotationValues{file.Group, file.Tags, file.Notes}})
	}
	return changes
}
