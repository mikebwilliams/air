package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

const inventoryFormatVersion = 1

// Inventory is immutable once saved. Review metadata lives outside this document.
// File paths are stable within a snapshot; scan-specific tasks will reference them.
type Inventory struct {
	FormatVersion int                  `json:"format_version"`
	SnapshotSHA   string               `json:"snapshot_sha"`
	WorkTree      string               `json:"work_tree"`
	BuildInputs   []InventoryBuildFile `json:"build_inputs"`
	Policy        InventoryPolicy      `json:"policy"`
	Files         []InventoryFile      `json:"files"`
	Commands      []InventoryCommand   `json:"commands"`
}

type InventoryBuildFile struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

type InventoryFile struct {
	Path            string   `json:"path"`
	BlobID          string   `json:"blob_id"`
	Mode            string   `json:"mode"`
	Bytes           int64    `json:"bytes"`
	Kind            string   `json:"kind"`
	Group           string   `json:"group"`
	Excluded        bool     `json:"excluded"`
	ExclusionReason string   `json:"exclusion_reason,omitempty"`
	CommandIDs      []int    `json:"command_ids"`
	Tags            []string `json:"tags"`
	Notes           []string `json:"notes"`
}

// Commands are retained as data, never executed by inventory building. IDs are
// one-based positions in Inventory.Commands, preserving multiple configurations.
type InventoryCommand struct {
	Directory string   `json:"directory"`
	File      string   `json:"file"`
	Arguments []string `json:"arguments,omitempty"`
	Command   string   `json:"command,omitempty"`
	Output    string   `json:"output,omitempty"`
}

type InventoryPolicy struct {
	Rules []InventoryRule `json:"rules"`
}

// Prefixes match an exact path or a directory and its descendants. Rules apply
// in order; annotations accumulate and the last explicit group/exclusion wins.
type InventoryRule struct {
	Prefix  string   `json:"prefix"`
	Group   string   `json:"group,omitempty"`
	Exclude *bool    `json:"exclude,omitempty"`
	Reason  string   `json:"reason,omitempty"`
	Tags    []string `json:"tags,omitempty"`
	Note    string   `json:"note,omitempty"`
}

type inventoryBuildOptions struct {
	CompileCommands      string
	Policy               InventoryPolicy
	AllowUnmatchedPolicy bool
}

func buildInventory(ctx context.Context, repository *GitRepository, options inventoryBuildOptions) (Inventory, error) {
	if err := validateInventoryPolicy(options.Policy); err != nil {
		return Inventory{}, err
	}
	sha, err := inventoryCheckoutSHA(ctx, repository)
	if err != nil {
		return Inventory{}, err
	}
	compilePath := options.CompileCommands
	if compilePath == "" {
		compilePath = "build/compile_commands.json"
	}
	if !filepath.IsAbs(compilePath) {
		compilePath = filepath.Join(repository.WorkTree, compilePath)
	}
	compilePath = filepath.Clean(compilePath)
	commands, fingerprint, err := readInventoryCommands(compilePath)
	if err != nil {
		return Inventory{}, err
	}
	inventory := Inventory{
		FormatVersion: inventoryFormatVersion, SnapshotSHA: sha, WorkTree: repository.WorkTree,
		BuildInputs: []InventoryBuildFile{fingerprint}, Policy: options.Policy,
		Commands: commands, Files: []InventoryFile{},
	}
	if inventory.Policy.Rules == nil {
		inventory.Policy.Rules = []InventoryRule{}
	}
	cachePath := filepath.Join(filepath.Dir(compilePath), "CMakeCache.txt")
	cache, err := os.ReadFile(cachePath)
	if err == nil {
		inventory.BuildInputs = append(inventory.BuildInputs, InventoryBuildFile{Path: cachePath, SHA256: inventoryHash(cache)})
	} else if !errors.Is(err, os.ErrNotExist) {
		return Inventory{}, fmt.Errorf("read CMake cache: %w", err)
	}
	commandIDs := make(map[string][]int)
	for index, command := range commands {
		commandIDs[command.File] = append(commandIDs[command.File], index+1)
	}
	// Read the committed tree rather than walking build outputs and untracked data.
	tree, err := repository.run(ctx, "ls-tree", "-r", "-l", "-z", "--full-tree", sha)
	if err != nil {
		return Inventory{}, err
	}
	matchedSources := 0
	for _, entry := range strings.Split(tree, "\x00") {
		if err := ctx.Err(); err != nil {
			return Inventory{}, err
		}
		if entry == "" {
			continue
		}
		file, err := parseInventoryTreeEntry(entry)
		if err != nil {
			return Inventory{}, err
		}
		if ids := commandIDs[filepath.Join(repository.WorkTree, filepath.FromSlash(file.Path))]; len(ids) > 0 {
			file.CommandIDs = ids
			if file.Kind == "source" {
				matchedSources++
			}
		}
		inventory.Files = append(inventory.Files, file)
	}
	if matchedSources == 0 {
		return Inventory{}, errors.New("no compilation commands match tracked C/C++ sources in this checkout; configure the build for the scan checkout")
	}
	inventory, err = inventoryWithPolicy(inventory, inventory.Policy)
	if err != nil {
		return Inventory{}, err
	}
	if unmatched := inventoryUnmatchedPrefixes(inventory); !options.AllowUnmatchedPolicy && len(unmatched) > 0 {
		return Inventory{}, fmt.Errorf("inventory policy prefix %q matches no tracked paths", unmatched[0])
	}
	// Catch accidental changes during inventory construction as well as on entry.
	if err := checkInventory(ctx, repository, inventory); err != nil {
		return Inventory{}, err
	}
	return inventory, nil
}

func inventoryCheckoutSHA(ctx context.Context, repository *GitRepository) (string, error) {
	sha, err := repository.ResolveCommit(ctx, "HEAD")
	if err != nil {
		return "", err
	}
	status, err := repository.run(ctx, "status", "--porcelain=v1", "--untracked-files=no", "--ignore-submodules=none")
	if err != nil {
		return "", err
	}
	if status != "" {
		return "", errors.New("scan checkout has tracked changes; use a clean, dedicated checkout")
	}
	return sha, nil
}

func checkInventory(ctx context.Context, repository *GitRepository, inventory Inventory) error {
	if repository.WorkTree != inventory.WorkTree {
		return fmt.Errorf("inventory belongs to checkout %s", inventory.WorkTree)
	}
	sha, err := inventoryCheckoutSHA(ctx, repository)
	if err != nil {
		return err
	}
	if sha != inventory.SnapshotSHA {
		return fmt.Errorf("checkout is at %s; inventory requires %s", shortSHA(sha), shortSHA(inventory.SnapshotSHA))
	}
	for _, input := range inventory.BuildInputs {
		data, err := os.ReadFile(input.Path)
		if err != nil {
			return fmt.Errorf("check build input %s: %w", input.Path, err)
		}
		if inventoryHash(data) != input.SHA256 {
			return fmt.Errorf("build input changed: %s; build a new inventory version", input.Path)
		}
	}
	return nil
}

func readInventoryCommands(filename string) ([]InventoryCommand, InventoryBuildFile, error) {
	data, err := os.ReadFile(filename)
	if err != nil {
		return nil, InventoryBuildFile{}, fmt.Errorf("read compilation database (configure CMake with CMAKE_EXPORT_COMPILE_COMMANDS=ON): %w", err)
	}
	var commands []InventoryCommand
	if err := json.Unmarshal(data, &commands); err != nil {
		return nil, InventoryBuildFile{}, fmt.Errorf("decode compilation database: %w", err)
	}
	if len(commands) == 0 {
		return nil, InventoryBuildFile{}, errors.New("compilation database contains no commands")
	}
	for index := range commands {
		command := &commands[index]
		if !filepath.IsAbs(command.Directory) || strings.ContainsRune(command.Directory, '\x00') {
			return nil, InventoryBuildFile{}, fmt.Errorf("compilation command %d requires an absolute directory", index+1)
		}
		if strings.TrimSpace(command.File) == "" || strings.ContainsRune(command.File, '\x00') {
			return nil, InventoryBuildFile{}, fmt.Errorf("compilation command %d has no valid file", index+1)
		}
		if len(command.Arguments) == 0 && strings.TrimSpace(command.Command) == "" ||
			len(command.Arguments) > 0 && strings.TrimSpace(command.Arguments[0]) == "" {
			return nil, InventoryBuildFile{}, fmt.Errorf("compilation command %d requires arguments or command", index+1)
		}
		command.Directory = filepath.Clean(command.Directory)
		if !filepath.IsAbs(command.File) {
			command.File = filepath.Join(command.Directory, command.File)
		}
		command.File = filepath.Clean(command.File)
	}
	return commands, InventoryBuildFile{Path: filename, SHA256: inventoryHash(data)}, nil
}

func parseInventoryTreeEntry(entry string) (InventoryFile, error) {
	header, name, found := strings.Cut(entry, "\t")
	fields := strings.Fields(header)
	if !found || len(fields) != 4 || !isHexObjectID(fields[2]) {
		return InventoryFile{}, errors.New("invalid Git tree entry")
	}
	file := InventoryFile{
		Path: name, Mode: fields[0], BlobID: fields[2], Kind: "other", Group: "root",
		CommandIDs: []int{}, Tags: []string{}, Notes: []string{},
	}
	if fields[3] != "-" {
		var err error
		file.Bytes, err = strconv.ParseInt(fields[3], 10, 64)
		if err != nil || file.Bytes < 0 {
			return InventoryFile{}, fmt.Errorf("invalid Git file size for %q", name)
		}
	}
	return inventoryFileDefaults(file)
}

func inventoryFileDefaults(file InventoryFile) (InventoryFile, error) {
	file.Kind, file.Group, file.ExclusionReason = "other", "root", ""
	file.Tags, file.Notes = []string{}, []string{}
	if strings.Contains(file.Path, "/") {
		file.Group = strings.SplitN(file.Path, "/", 2)[0]
	}
	switch file.Mode {
	case "160000":
		file.Kind, file.ExclusionReason = "submodule", "submodule contents are outside this inventory"
	case "120000":
		file.Kind, file.ExclusionReason = "symlink", "symbolic link"
	case "100644", "100755":
		switch strings.ToLower(path.Ext(file.Path)) {
		case ".c", ".cc", ".cpp", ".cxx", ".c++":
			file.Kind = "source"
		case ".h", ".hh", ".hpp", ".hxx", ".h++", ".inl", ".ipp", ".tpp":
			file.Kind = "header"
		default:
			file.ExclusionReason = "outside C/C++ review scope"
		}
	default:
		return InventoryFile{}, fmt.Errorf("unsupported Git file mode %q", file.Mode)
	}
	for _, rule := range inventoryDefaultExclusions() {
		if (file.Kind == "source" || file.Kind == "header") && inventoryPrefixMatches(file.Path, rule.Prefix) {
			file.ExclusionReason = rule.Reason
		}
	}
	file.Excluded = file.ExclusionReason != ""
	return file, nil
}

func validateInventoryPolicy(policy InventoryPolicy) error {
	for index, rule := range policy.Rules {
		if rule.Prefix == "" || path.IsAbs(rule.Prefix) || path.Clean(rule.Prefix) != rule.Prefix ||
			rule.Prefix == ".." || strings.HasPrefix(rule.Prefix, "../") || strings.ContainsAny(rule.Prefix, "\x00\\*?[") {
			return fmt.Errorf("policy rule %d: prefix must be a clean repository-relative path, or . for all paths", index+1)
		}
		if rule.Exclude != nil && *rule.Exclude && strings.TrimSpace(rule.Reason) == "" {
			return fmt.Errorf("policy rule %d: exclusions require a reason", index+1)
		}
		if rule.Group != "" && strings.TrimSpace(rule.Group) == "" {
			return fmt.Errorf("policy rule %d: group must not be blank", index+1)
		}
	}
	return nil
}

func inventoryPrefixMatches(filename, prefix string) bool {
	return prefix == "." || filename == prefix || strings.HasPrefix(filename, prefix+"/")
}

func applyInventoryRule(file *InventoryFile, rule InventoryRule) {
	if rule.Group != "" {
		file.Group = rule.Group
	}
	if rule.Exclude != nil && (file.Kind == "source" || file.Kind == "header") {
		file.Excluded = *rule.Exclude
		file.ExclusionReason = ""
		if *rule.Exclude {
			file.ExclusionReason = rule.Reason
		}
	}
	for _, tag := range rule.Tags {
		if !slices.Contains(file.Tags, tag) {
			file.Tags = append(file.Tags, tag)
		}
	}
	if rule.Note != "" {
		file.Notes = append(file.Notes, rule.Note)
	}
}

func inventoryHash(data []byte) string {
	return fmt.Sprintf("%x", sha256.Sum256(data))
}
