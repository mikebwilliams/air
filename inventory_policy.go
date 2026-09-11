package main

import (
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"strings"
	"text/tabwriter"
)

func inventoryDefaultExclusions() []InventoryRule {
	exclude := true
	return []InventoryRule{
		{Prefix: "thirdparty", Exclude: &exclude, Reason: "third-party code (default policy)"},
		{Prefix: "build", Exclude: &exclude, Reason: "build output (default policy)"},
	}
}

// Reapply policy using only saved facts. No source/build access or compiler work
// is needed, and slices shared with the original document must remain unchanged.
func inventoryWithPolicy(inventory Inventory, policy InventoryPolicy) (Inventory, error) {
	if err := validateInventoryPolicy(policy); err != nil {
		return Inventory{}, err
	}
	updated := inventory
	updated.Policy = InventoryPolicy{Rules: append([]InventoryRule{}, policy.Rules...)}
	updated.Files = make([]InventoryFile, 0, len(inventory.Files))
	for _, file := range inventory.Files {
		// Reconstruct defaults from the original Git facts, clearing all old policy
		// effects (including annotations) before applying the complete new policy.
		fresh, err := inventoryFileDefaults(file)
		if err != nil {
			return Inventory{}, err
		}
		fresh.CommandIDs = slices.Clone(file.CommandIDs)
		for _, rule := range policy.Rules {
			if inventoryPrefixMatches(file.Path, rule.Prefix) {
				applyInventoryRule(&fresh, rule)
			}
		}
		updated.Files = append(updated.Files, fresh)
	}
	return updated, nil
}

type InventoryScopeChange struct {
	Path   string `json:"path"`
	Before string `json:"before"`
	After  string `json:"after"`
	Reason string `json:"reason,omitempty"`
}

type InventoryPolicyResult struct {
	PreviousID string                 `json:"previous_id"`
	Record     InventoryRecord        `json:"record"`
	Changes    []InventoryScopeChange `json:"changes"`
}

// An explicit include/exclude takes precedence over previous rules. Replacing
// an identical trailing rule is a no-op, so repeated commands reuse the version.
func editInventoryScope(inventory Inventory, prefixes []string, exclude bool, reason string) (Inventory, []InventoryScopeChange, error) {
	policy := InventoryPolicy{Rules: append([]InventoryRule{}, inventory.Policy.Rules...)}
	for _, prefix := range prefixes {
		rule := InventoryRule{Prefix: prefix, Exclude: &exclude, Reason: reason}
		if err := validateInventoryPolicy(InventoryPolicy{Rules: []InventoryRule{rule}}); err != nil {
			return Inventory{}, nil, err
		}
		matched := false
		for _, file := range inventory.Files {
			if (file.Kind == "source" || file.Kind == "header") && inventoryPrefixMatches(file.Path, prefix) {
				matched = true
				break
			}
		}
		if !matched {
			return Inventory{}, nil, fmt.Errorf("prefix %q matches no C/C++ inventory files", prefix)
		}
		// Remove previous scope-only rules for this exact prefix. Rules carrying
		// groups/tags/notes retain those annotations, with their scope fields cleared.
		rules := make([]InventoryRule, 0, len(policy.Rules)+1)
		for _, previous := range policy.Rules {
			if previous.Prefix == prefix && previous.Exclude != nil {
				previous.Exclude, previous.Reason = nil, ""
				if previous.Group == "" && len(previous.Tags) == 0 && previous.Note == "" {
					continue
				}
			}
			rules = append(rules, previous)
		}
		policy.Rules = append(rules, rule)
	}
	updated, err := inventoryWithPolicy(inventory, policy)
	if err != nil {
		return Inventory{}, nil, err
	}
	return updated, inventoryScopeChanges(inventory, updated), nil
}

func inventoryScopeChanges(before, after Inventory) []InventoryScopeChange {
	changes := []InventoryScopeChange{}
	for index, file := range after.Files {
		old := before.Files[index]
		if old.Excluded == file.Excluded && old.ExclusionReason == file.ExclusionReason {
			continue
		}
		status := func(excluded bool) string {
			if excluded {
				return "excluded"
			}
			return "included"
		}
		changes = append(changes, InventoryScopeChange{Path: file.Path, Before: status(old.Excluded), After: status(file.Excluded), Reason: file.ExclusionReason})
	}
	return changes
}

type InventoryExclusionRule struct {
	Source    string `json:"source"`
	Prefix    string `json:"prefix"`
	Action    string `json:"action"`
	Reason    string `json:"reason,omitempty"`
	Matched   int    `json:"matched"`
	Effective int    `json:"effective"`
}

func inventoryExclusionRules(inventory Inventory) []InventoryExclusionRule {
	defaults := inventoryDefaultExclusions()
	rules := append(defaults, inventory.Policy.Rules...)
	result := []InventoryExclusionRule{}
	for index, rule := range rules {
		if rule.Exclude == nil {
			continue
		}
		row := InventoryExclusionRule{Source: "policy", Prefix: rule.Prefix, Action: "include", Reason: rule.Reason}
		if index < len(defaults) {
			row.Source = "default"
		}
		if *rule.Exclude {
			row.Action = "exclude"
		}
		for _, file := range inventory.Files {
			if (file.Kind != "source" && file.Kind != "header") || !inventoryPrefixMatches(file.Path, rule.Prefix) {
				continue
			}
			row.Matched++
			overridden := false
			for _, later := range rules[index+1:] {
				if later.Exclude != nil && inventoryPrefixMatches(file.Path, later.Prefix) {
					overridden = true
					break
				}
			}
			if !overridden {
				row.Effective++
			}
		}
		result = append(result, row)
	}
	return result
}

func inventoryUnmatchedPrefixes(inventory Inventory) []string {
	unmatched := []string{}
	for _, rule := range inventory.Policy.Rules {
		matched := false
		for _, file := range inventory.Files {
			if inventoryPrefixMatches(file.Path, rule.Prefix) {
				matched = true
				break
			}
		}
		if !matched && !slices.Contains(unmatched, rule.Prefix) {
			unmatched = append(unmatched, rule.Prefix)
		}
	}
	return unmatched
}

func readInventoryPolicy(input io.Reader) (InventoryPolicy, error) {
	var policy *InventoryPolicy
	decoder := json.NewDecoder(input)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&policy); err != nil {
		return InventoryPolicy{}, fmt.Errorf("decode inventory policy: %w", err)
	}
	if err := ensureJSONEOF(decoder); err != nil {
		return InventoryPolicy{}, fmt.Errorf("decode inventory policy: %w", err)
	}
	if policy == nil {
		return InventoryPolicy{}, fmt.Errorf("inventory policy must be a JSON object")
	}
	if err := validateInventoryPolicy(*policy); err != nil {
		return InventoryPolicy{}, err
	}
	return *policy, nil
}

func printInventoryPolicyResult(output io.Writer, result InventoryPolicyResult) error {
	fmt.Fprintf(output, "Current inventory: %s (%s)\n", result.Record.ID[:12], inventoryReviewLabel(result.Record.ReviewedAt != nil))
	fmt.Fprintf(output, "Scope changes: %d files. Policy saved for future inventory builds.\n", len(result.Changes))
	writer := tabwriter.NewWriter(output, 0, 4, 2, ' ', 0)
	if len(result.Changes) > 0 {
		fmt.Fprintln(writer, "PATH\tBEFORE\tAFTER\tREASON")
	}
	for _, change := range result.Changes[:min(20, len(result.Changes))] {
		fmt.Fprintf(writer, "%q\t%s\t%s\t%q\n", change.Path, change.Before, change.After, change.Reason)
	}
	if err := writer.Flush(); err != nil {
		return err
	}
	if len(result.Changes) > 20 {
		fmt.Fprintln(output, "Showing the first 20 changed files; use --json for the complete list.")
	}
	if unmatched := inventoryUnmatchedPrefixes(result.Record.Inventory); len(unmatched) > 0 {
		fmt.Fprintf(output, "Policy prefixes without matches in this snapshot: %s\n", strings.Join(unmatched, ", "))
	}
	return nil
}
