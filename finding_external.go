package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

type findingCommandBuilder func(context.Context, Finding) (*exec.Cmd, error)
type findingPreviewLoader func(context.Context, Finding) (findingDiffPreview, error)

type findingExternalCommands struct {
	diff    findingCommandBuilder
	open    findingCommandBuilder
	preview findingPreviewLoader
}

func newFindingExternalCommands(repository *GitRepository, commandContext commandContextFunc) findingExternalCommands {
	if commandContext == nil {
		commandContext = exec.CommandContext
	}
	return findingExternalCommands{
		diff: func(ctx context.Context, finding Finding) (*exec.Cmd, error) {
			return buildFindingDiffCommand(ctx, repository, finding, commandContext)
		},
		open: func(ctx context.Context, finding Finding) (*exec.Cmd, error) {
			return buildFindingOpenCommand(ctx, repository, finding, commandContext)
		},
		preview: func(ctx context.Context, finding Finding) (findingDiffPreview, error) {
			return loadFindingDiffPreview(ctx, repository, finding)
		},
	}
}

func buildFindingDiffCommand(
	ctx context.Context,
	repository *GitRepository,
	finding Finding,
	commandContext commandContextFunc,
) (*exec.Cmd, error) {
	if repository == nil {
		return nil, errors.New("no Git repository is available")
	}
	metadata, err := repository.CommitMetadata(ctx, finding.IntroducedSHA)
	if err != nil {
		return nil, fmt.Errorf("locate introducing commit for finding #%d: %w", finding.ID, err)
	}
	if metadata.ParentSHA == "" {
		return nil, fmt.Errorf("finding #%d was introduced by a root commit, which has no parent to diff", finding.ID)
	}
	command := commandContext(ctx, "git",
		"-C", repository.WorkTree,
		"--literal-pathspecs",
		"difftool", "--no-prompt",
		metadata.ParentSHA, metadata.SHA,
		"--",
	)
	command.Env = append(os.Environ(), "GIT_OPTIONAL_LOCKS=0")
	return command, nil
}

func buildFindingOpenCommand(
	ctx context.Context,
	repository *GitRepository,
	finding Finding,
	commandContext commandContextFunc,
) (*exec.Cmd, error) {
	if repository == nil {
		return nil, errors.New("no Git repository is available")
	}
	if finding.File == nil {
		return nil, fmt.Errorf("finding #%d has no file location", finding.ID)
	}
	if err := validateRepositoryPath(*finding.File); err != nil {
		return nil, fmt.Errorf("finding #%d has an invalid file location: %w", finding.ID, err)
	}
	filename := filepath.Join(repository.WorkTree, filepath.FromSlash(*finding.File))
	info, err := os.Stat(filename)
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("%s no longer exists in the working tree", *finding.File)
	}
	if err != nil {
		return nil, fmt.Errorf("inspect %s: %w", *finding.File, err)
	}
	if info.IsDir() {
		return nil, fmt.Errorf("%s is a directory, not a file", *finding.File)
	}
	editorOutput, err := repository.run(ctx, "var", "GIT_EDITOR")
	if err != nil {
		return nil, fmt.Errorf("find Git editor: %w", err)
	}
	editor := strings.TrimSpace(editorOutput)
	if editor == "" || strings.ContainsRune(editor, '\x00') {
		return nil, errors.New("Git returned an invalid editor command")
	}
	arguments := editorLocationArguments(editor, filename, finding.Line)
	// Git editor values are shell commands. The positional arguments keep the
	// repository filename and optional line selector out of the command string.
	command := commandContext(ctx, "sh", append([]string{"-c", "exec " + editor + ` "$@"`, "air-editor"}, arguments...)...)
	command.Dir = repository.WorkTree
	command.Env = os.Environ()
	return command, nil
}

func editorLocationArguments(editor, filename string, line *int) []string {
	if line == nil {
		return []string{filename}
	}
	lineValue := strconv.Itoa(*line)
	name := editorExecutableName(editor)
	switch name {
	case "code", "code-insiders", "codium":
		return []string{"--goto", filename + ":" + lineValue}
	case "vi", "vim", "nvim", "view", "gvim", "nano", "emacs", "emacsclient":
		return []string{"+" + lineValue, filename}
	case "hx", "helix", "subl", "zed":
		return []string{filename + ":" + lineValue}
	case "idea", "idea.sh", "kate":
		return []string{"--line", lineValue, filename}
	default:
		return []string{filename}
	}
}

func editorExecutableName(editor string) string {
	fields := strings.Fields(editor)
	if len(fields) == 0 {
		return ""
	}
	executable := strings.Trim(fields[0], `"'`)
	return strings.ToLower(filepath.Base(executable))
}

func runFindingDiff(ctx context.Context, args []string, environment cliEnvironment) error {
	return runFindingExternal(ctx, "diff", args, environment)
}

func runFindingOpen(ctx context.Context, args []string, environment cliEnvironment) error {
	return runFindingExternal(ctx, "open", args, environment)
}

func runFindingExternal(ctx context.Context, action string, args []string, environment cliEnvironment) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: air finding %s <id>", action)
	}
	id, err := parseFindingID(args[0])
	if err != nil {
		return err
	}
	repository, store, closeStore, err := openRepositoryStore(ctx, environment.Cwd)
	if err != nil {
		return err
	}
	defer closeStore()
	finding, err := store.Finding(ctx, id)
	if err != nil {
		return err
	}
	commands := newFindingExternalCommands(repository, environment.ExternalCommand)
	builder := commands.diff
	if action == "open" {
		builder = commands.open
	}
	command, err := builder(ctx, finding)
	if err != nil {
		return err
	}
	attachCommandIO(command, environment.Stdin, environment.Stdout, environment.Stderr)
	if err := command.Run(); err != nil {
		return fmt.Errorf("%s finding #%d: %w", action, id, err)
	}
	return nil
}

func attachCommandIO(command *exec.Cmd, stdin io.Reader, stdout, stderr io.Writer) {
	command.Stdin = stdin
	command.Stdout = stdout
	command.Stderr = stderr
}
