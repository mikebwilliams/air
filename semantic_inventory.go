package main

import (
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

var semanticClangdFlags = []string{"--enable-config=false", "--background-index=false", "--clang-tidy=false", "--pch-storage=memory", "--header-insertion=never", "--log=error"}

type semanticProfile struct {
	ID               string               `json:"id"`
	BaseID           string               `json:"base_id"`
	FormatVersion    int                  `json:"format_version"`
	SnapshotSHA      string               `json:"snapshot_sha"`
	WorkTree         string               `json:"work_tree"`
	BuildInputs      []InventoryBuildFile `json:"build_inputs"`
	Clangd           string               `json:"clangd"`
	ClangdVersion    string               `json:"clangd_version"`
	ClangdSHA256     string               `json:"clangd_sha256"`
	Flags            []string             `json:"flags"`
	Environment      map[string]string    `json:"environment"`
	CommandPolicy    string               `json:"command_policy"`
	DependencyPolicy string               `json:"dependency_policy,omitempty"`
}

func semanticBaseID(inventory Inventory) string {
	data, _ := json.Marshal(struct {
		SHA, WorkTree string
		Inputs        []InventoryBuildFile
	}{inventory.SnapshotSHA, inventory.WorkTree, inventory.BuildInputs})
	return inventoryHash(data)
}

func makeSemanticProfile(ctx context.Context, inventory Inventory, executable string) (semanticProfile, error) {
	executable, err := exec.LookPath(executable)
	if err != nil {
		return semanticProfile{}, err
	}
	executable, err = filepath.Abs(executable)
	if err != nil {
		return semanticProfile{}, err
	}
	file, err := os.Open(executable)
	if err != nil {
		return semanticProfile{}, err
	}
	hash := sha256.New()
	_, err = io.Copy(hash, file)
	file.Close()
	if err != nil {
		return semanticProfile{}, err
	}
	versionContext, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	version, err := exec.CommandContext(versionContext, executable, "--version").CombinedOutput()
	if err != nil {
		return semanticProfile{}, fmt.Errorf("clangd version: %w: %s", err, version)
	}
	profile := semanticProfile{BaseID: semanticBaseID(inventory), FormatVersion: 1, SnapshotSHA: inventory.SnapshotSHA, WorkTree: inventory.WorkTree,
		BuildInputs: inventory.BuildInputs, Clangd: executable, ClangdVersion: strings.TrimSpace(string(version)), ClangdSHA256: fmt.Sprintf("%x", hash.Sum(nil)),
		Flags: append([]string{}, semanticClangdFlags...), Environment: map[string]string{}, CommandPolicy: "first saved source command; header borrows an observed includer's source command"}
	profile.DependencyPolicy = "fingerprint direct includes and explicit forced includes; transitive dependency changes require --refresh"
	for _, name := range []string{"PATH", "CPATH", "CPLUS_INCLUDE_PATH", "C_INCLUDE_PATH", "OBJC_INCLUDE_PATH", "SDKROOT", "INCLUDE", "CLANGD_FLAGS"} {
		if value, present := os.LookupEnv(name); present {
			profile.Environment[name] = value
		}
	}
	data, err := json.Marshal(profile)
	if err != nil {
		return semanticProfile{}, err
	}
	profile.ID = inventoryHash(data)
	return profile, nil
}

// LSP coordinates are zero-based with an exclusive end. UTF-8 is negotiated;
// byte offsets are also persisted so plans don't need live checkout contents.
type semanticPosition struct {
	Line      int `json:"line"`
	Character int `json:"character"`
}
type semanticRange struct {
	Start semanticPosition `json:"start"`
	End   semanticPosition `json:"end"`
}
type semanticSymbol struct {
	Name           string           `json:"name"`
	Detail         string           `json:"detail,omitempty"`
	Kind           int              `json:"kind"`
	Range          semanticRange    `json:"range"`
	SelectionRange semanticRange    `json:"selectionRange"`
	StartByte      int64            `json:"start_byte"`
	EndByte        int64            `json:"end_byte"`
	Children       []semanticSymbol `json:"children,omitempty"`
}
type semanticLink struct {
	Range  semanticRange `json:"range"`
	Target string        `json:"target"`
	Path   string        `json:"path,omitempty"`
	SHA256 string        `json:"sha256,omitempty"`
	Error  string        `json:"error,omitempty"`
}
type semanticDiagnostic struct {
	Range    semanticRange   `json:"range"`
	Severity int             `json:"severity"`
	Code     json.RawMessage `json:"code,omitempty"`
	Message  string          `json:"message"`
}
type semanticFileResult struct {
	ID               string               `json:"id"`
	ProfileID        string               `json:"profile_id"`
	Path             string               `json:"path"`
	BlobID           string               `json:"blob_id"`
	Bytes            int64                `json:"bytes"`
	CommandID        int                  `json:"command_id,omitempty"`
	CommandSource    string               `json:"command_source,omitempty"`
	CommandArguments []string             `json:"command_arguments,omitempty"`
	Status           string               `json:"status"`
	Error            string               `json:"error,omitempty"`
	Symbols          []semanticSymbol     `json:"symbols"`
	Includes         []semanticLink       `json:"includes"`
	Diagnostics      []semanticDiagnostic `json:"diagnostics"`
	BuildInputs      []InventoryBuildFile `json:"build_inputs,omitempty"`
}
type semanticSnapshot struct {
	Profile semanticProfile      `json:"profile"`
	Files   []semanticFileResult `json:"files"`
}

func semanticSourceText(inventory Inventory, file InventoryFile) ([]byte, error) {
	data, err := os.ReadFile(filepath.Join(inventory.WorkTree, filepath.FromSlash(file.Path)))
	if err != nil {
		return nil, err
	}
	header := fmt.Appendf(nil, "blob %d\x00", len(data))
	var actual string
	if len(file.BlobID) == 40 {
		hash := sha1.New()
		hash.Write(header)
		hash.Write(data)
		actual = fmt.Sprintf("%x", hash.Sum(nil))
	} else {
		hash := sha256.New()
		hash.Write(header)
		hash.Write(data)
		actual = fmt.Sprintf("%x", hash.Sum(nil))
	}
	if actual != file.BlobID {
		return nil, fmt.Errorf("source %s changed from the inventory blob", file.Path)
	}
	if !utf8.Valid(data) {
		return nil, fmt.Errorf("source %s is not valid UTF-8; refusing to alter text during LSP encoding", file.Path)
	}
	return data, nil
}

func semanticSymbolOffsets(symbols []semanticSymbol, contents []byte) error {
	starts := []int{0}
	for i, b := range contents {
		if b == '\n' {
			starts = append(starts, i+1)
		}
	}
	offset := func(position semanticPosition) (int64, error) {
		if position.Line < 0 || position.Line >= len(starts) || position.Character < 0 {
			return 0, fmt.Errorf("invalid symbol position %+v", position)
		}
		start := starts[position.Line]
		end := len(contents)
		if position.Line+1 < len(starts) {
			end = starts[position.Line+1] - 1
		}
		if position.Character > end-start {
			return 0, fmt.Errorf("symbol column outside source line: %+v", position)
		}
		return int64(start + position.Character), nil
	}
	var visit func([]semanticSymbol) error
	visit = func(items []semanticSymbol) error {
		for i := range items {
			var err error
			items[i].StartByte, err = offset(items[i].Range.Start)
			if err != nil {
				return err
			}
			items[i].EndByte, err = offset(items[i].Range.End)
			if err != nil {
				return err
			}
			if items[i].EndByte < items[i].StartByte {
				return fmt.Errorf("reversed symbol range")
			}
			if err := visit(items[i].Children); err != nil {
				return err
			}
		}
		return nil
	}
	return visit(symbols)
}

func semanticResolvedLinks(worktree string, links []semanticLink) []semanticLink {
	for i := range links {
		filename, err := clangdPath(links[i].Target)
		if err != nil {
			links[i].Error = err.Error()
			continue
		}
		relative, err := filepath.Rel(worktree, filename)
		if err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			links[i].Path = filepath.ToSlash(relative)
		} else {
			links[i].Path = filename
		}
		data, err := os.ReadFile(filename)
		if err != nil {
			links[i].Error = err.Error()
		} else {
			links[i].SHA256 = inventoryHash(data)
		}
	}
	return links
}

// Compilation database command strings use shell quoting, but are data. This
// tokenizer performs no expansion, substitution, globbing, or shell execution.
func semanticCommandArguments(command InventoryCommand) ([]string, error) {
	if len(command.Arguments) > 0 {
		return append([]string{}, command.Arguments...), nil
	}
	var words []string
	var word strings.Builder
	quote := rune(0)
	escaped, active := false, false
	for _, r := range command.Command {
		if escaped {
			if quote == '"' && !strings.ContainsRune("\\\"$`\n", r) {
				word.WriteRune('\\')
			}
			if r != '\n' {
				word.WriteRune(r)
			}
			escaped = false
			active = true
			continue
		}
		if r == '\\' && quote != '\'' {
			escaped = true
			active = true
			continue
		}
		if quote != 0 {
			if r == quote {
				quote = 0
			} else {
				word.WriteRune(r)
			}
			continue
		}
		switch r {
		case '\'', '"':
			quote = r
			active = true
		case ' ', '\t', '\r', '\n':
			if active {
				words = append(words, word.String())
				word.Reset()
				active = false
			}
		default:
			word.WriteRune(r)
			active = true
		}
	}
	if quote != 0 || escaped {
		return nil, fmt.Errorf("unterminated quoting in compilation command")
	}
	if active {
		words = append(words, word.String())
	}
	if len(words) == 0 {
		return nil, fmt.Errorf("empty compilation command")
	}
	return words, nil
}

func semanticHeaderCommand(source InventoryCommand, filename string) (InventoryCommand, error) {
	args, err := semanticCommandArguments(source)
	if err != nil {
		return InventoryCommand{}, err
	}
	found := false
	for i := 1; i < len(args); i++ {
		candidate := args[i]
		if !filepath.IsAbs(candidate) {
			candidate = filepath.Join(source.Directory, candidate)
		}
		if filepath.Clean(candidate) == source.File {
			args[i] = filename
			found = true
		}
	}
	if !found {
		return InventoryCommand{}, fmt.Errorf("cannot locate source input in command borrowed from %s", source.File)
	}
	language := "c++-header"
	if strings.EqualFold(filepath.Ext(source.File), ".c") {
		language = "c-header"
	}
	// The explicit language precedes all inputs, including a possible -- marker.
	args = append([]string{args[0], "-x", language}, args[1:]...)
	return InventoryCommand{File: filename, Directory: source.Directory, Arguments: args}, nil
}

func semanticSymbolCount(symbols []semanticSymbol) int {
	count := len(symbols)
	for _, symbol := range symbols {
		count += semanticSymbolCount(symbol.Children)
	}
	return count
}
func (snapshot *semanticSnapshot) byPath() map[string]semanticFileResult {
	result := map[string]semanticFileResult{}
	if snapshot != nil {
		for _, file := range snapshot.Files {
			result[file.Path] = file
		}
	}
	return result
}
func semanticSortedResults(files map[string]semanticFileResult) []semanticFileResult {
	result := make([]semanticFileResult, 0, len(files))
	for _, file := range files {
		result = append(result, file)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Path < result[j].Path })
	return result
}
