package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strconv"
	"strings"
)

type GitRepository struct {
	WorkTree  string
	CommonDir string
}

type DiffResult struct {
	Text        string
	TextFiles   []string
	BinaryFiles []string
	Empty       bool
	Oversized   bool
}

func DiscoverGitRepository(ctx context.Context, start string) (*GitRepository, error) {
	workTree, err := runDiscoveryGit(ctx, start, "rev-parse", "--show-toplevel")
	if err != nil {
		return nil, fmt.Errorf("not inside a Git repository: %w", err)
	}
	commonDir, err := runDiscoveryGit(ctx, start, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return nil, fmt.Errorf("locate Git common directory: %w", err)
	}
	workTree = strings.TrimSpace(workTree)
	commonDir = strings.TrimSpace(commonDir)
	if !filepath.IsAbs(workTree) || !filepath.IsAbs(commonDir) {
		return nil, errors.New("Git returned a non-absolute repository path")
	}
	return &GitRepository{WorkTree: filepath.Clean(workTree), CommonDir: filepath.Clean(commonDir)}, nil
}

func runDiscoveryGit(ctx context.Context, dir string, args ...string) (string, error) {
	cmdArgs := append([]string{"-C", dir}, args...)
	cmd := exec.CommandContext(ctx, "git", cmdArgs...)
	cmd.Env = append(os.Environ(), "GIT_OPTIONAL_LOCKS=0", "GIT_TERMINAL_PROMPT=0")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", commandError("git", cmdArgs, stderr.String(), err)
	}
	return stdout.String(), nil
}

func (r *GitRepository) DatabasePath() string {
	return filepath.Join(r.StateDirectory(), "reviews.sqlite")
}

func (r *GitRepository) LockPath() string {
	return filepath.Join(r.StateDirectory(), "scan.lock")
}

func (r *GitRepository) StateDirectory() string {
	return filepath.Join(r.CommonDir, "air")
}

func (r *GitRepository) ResolveCommit(ctx context.Context, revision string) (string, error) {
	if revision == "" || strings.ContainsRune(revision, '\x00') {
		return "", errors.New("empty or invalid revision")
	}
	out, err := r.run(ctx, "rev-parse", "--verify", "--end-of-options", revision+"^{commit}")
	if err != nil {
		return "", fmt.Errorf("resolve %q as a commit: %w", revision, err)
	}
	sha := strings.TrimSpace(out)
	if !isHexObjectID(sha) {
		return "", fmt.Errorf("Git returned invalid object ID %q", sha)
	}
	return sha, nil
}

func (r *GitRepository) MasterSHA(ctx context.Context) (string, error) {
	sha, err := r.ResolveCommit(ctx, masterRef)
	if err != nil {
		return "", fmt.Errorf("%s does not exist: %w", masterRef, err)
	}
	return sha, nil
}

func (r *GitRepository) MasterHistory(ctx context.Context) ([]string, error) {
	if _, err := r.MasterSHA(ctx); err != nil {
		return nil, err
	}
	out, err := r.run(ctx, "rev-list", "--first-parent", masterRef)
	if err != nil {
		return nil, fmt.Errorf("enumerate master history: %w", err)
	}
	return parseObjectIDs(out)
}

func (r *GitRepository) IsOnMasterFirstParent(ctx context.Context, sha string) (bool, error) {
	history, err := r.MasterHistory(ctx)
	if err != nil {
		return false, err
	}
	for _, candidate := range history {
		if candidate == sha {
			return true, nil
		}
	}
	return false, nil
}

func (r *GitRepository) EnumerateDefault(ctx context.Context, startSHA string) ([]string, error) {
	if err := r.validateHistoryEndpoints(ctx, startSHA, ""); err != nil {
		return nil, err
	}
	out, err := r.run(ctx, "rev-list", "--reverse", "--first-parent", startSHA+".."+masterRef)
	if err != nil {
		return nil, fmt.Errorf("enumerate commits: %w", err)
	}
	return parseObjectIDs(out)
}

func (r *GitRepository) EnumerateRange(ctx context.Context, revisionRange string) ([]string, error) {
	fromRevision, toRevision, err := splitTwoDotRange(revisionRange)
	if err != nil {
		return nil, err
	}
	fromSHA, err := r.ResolveCommit(ctx, fromRevision)
	if err != nil {
		return nil, err
	}
	toSHA, err := r.ResolveCommit(ctx, toRevision)
	if err != nil {
		return nil, err
	}
	if err := r.validateHistoryEndpoints(ctx, fromSHA, toSHA); err != nil {
		return nil, err
	}
	out, err := r.run(ctx, "rev-list", "--reverse", "--first-parent", fromSHA+".."+toSHA)
	if err != nil {
		return nil, fmt.Errorf("enumerate range: %w", err)
	}
	return parseObjectIDs(out)
}

func splitTwoDotRange(value string) (string, string, error) {
	if strings.Contains(value, "...") || strings.Count(value, "..") != 1 {
		return "", "", errors.New("range must have the form <from>..<to>")
	}
	parts := strings.SplitN(value, "..", 2)
	if parts[0] == "" || parts[1] == "" {
		return "", "", errors.New("range endpoints must not be empty")
	}
	return parts[0], parts[1], nil
}

func (r *GitRepository) validateHistoryEndpoints(ctx context.Context, fromSHA, toSHA string) error {
	history, err := r.MasterHistory(ctx)
	if err != nil {
		return err
	}
	positions := make(map[string]int, len(history))
	for i, sha := range history {
		positions[sha] = i
	}
	fromPosition, ok := positions[fromSHA]
	if !ok {
		return fmt.Errorf("commit %s is not on the first-parent history of master", shortSHA(fromSHA))
	}
	if toSHA == "" {
		return nil
	}
	toPosition, ok := positions[toSHA]
	if !ok {
		return fmt.Errorf("commit %s is not on the first-parent history of master", shortSHA(toSHA))
	}
	// History is newest first, so the lower endpoint must have the greater
	// index (or the same index for an empty range).
	if fromPosition < toPosition {
		return fmt.Errorf("range start %s does not precede %s on master", shortSHA(fromSHA), shortSHA(toSHA))
	}
	return nil
}

func (r *GitRepository) CommitMetadata(ctx context.Context, sha string) (CommitMetadata, error) {
	format := "%H%x00%P%x00%aN <%aE>%x00%aI%x00%B"
	out, err := r.run(ctx, "show", "-s", "--no-show-signature", "--format="+format, sha)
	if err != nil {
		return CommitMetadata{}, fmt.Errorf("read commit %s: %w", shortSHA(sha), err)
	}
	parts := strings.SplitN(out, "\x00", 5)
	if len(parts) != 5 {
		return CommitMetadata{}, fmt.Errorf("unexpected metadata for commit %s", shortSHA(sha))
	}
	parents := strings.Fields(parts[1])
	if len(parents) == 0 {
		return CommitMetadata{}, fmt.Errorf("commit %s has no first parent", shortSHA(sha))
	}
	return CommitMetadata{
		SHA:       strings.TrimSpace(parts[0]),
		ParentSHA: parents[0],
		Author:    parts[2],
		Date:      parts[3],
		Message:   strings.TrimSpace(parts[4]),
	}, nil
}

func (r *GitRepository) CommitDiff(ctx context.Context, parentSHA, sha string) (DiffResult, error) {
	numstat, err := r.runBytes(ctx, 0, "diff", "--no-ext-diff", "--no-textconv", "--numstat", "-z", "--no-renames", parentSHA, sha)
	if err != nil {
		return DiffResult{}, fmt.Errorf("inspect diff for %s: %w", shortSHA(sha), err)
	}
	entries, err := parseNumstat(numstat.data)
	if err != nil {
		return DiffResult{}, fmt.Errorf("inspect diff for %s: %w", shortSHA(sha), err)
	}
	if len(entries) == 0 {
		return DiffResult{Empty: true}, nil
	}
	var textPaths, binaryPaths []string
	for _, entry := range entries {
		if entry.binary {
			binaryPaths = append(binaryPaths, entry.path)
		} else {
			textPaths = append(textPaths, entry.path)
		}
	}
	if len(textPaths) == 0 {
		return DiffResult{BinaryFiles: binaryPaths}, nil
	}
	args := []string{"diff", "--no-ext-diff", "--no-textconv", "--no-color", "--no-renames", parentSHA, sha, "--"}
	args = append(args, textPaths...)
	diff, err := r.runBytes(ctx, maxDiffBytes, args...)
	if err != nil {
		return DiffResult{}, fmt.Errorf("read diff for %s: %w", shortSHA(sha), err)
	}
	return DiffResult{
		Text:        string(diff.data),
		TextFiles:   textPaths,
		BinaryFiles: binaryPaths,
		Oversized:   diff.exceeded,
	}, nil
}

func (r *GitRepository) ReadFile(ctx context.Context, sha, repositoryPath string) (string, error) {
	if err := validateRepositoryPath(repositoryPath); err != nil {
		return "", err
	}
	out, err := r.runBytes(ctx, maxToolBytes, "show", sha+":"+repositoryPath)
	if err != nil {
		return "", err
	}
	return formatLimitedOutput(out), nil
}

func (r *GitRepository) Grep(ctx context.Context, sha, pattern, repositoryPath string) (string, error) {
	if pattern == "" || len(pattern) > 256 || strings.ContainsRune(pattern, '\x00') {
		return "", errors.New("grep pattern must contain 1 to 256 bytes")
	}
	args := []string{"grep", "-n", "-F", "-e", pattern, sha}
	if repositoryPath != "" {
		if err := validateRepositoryPath(repositoryPath); err != nil {
			return "", err
		}
		args = append(args, "--", repositoryPath)
	}
	out, err := r.runBytesAllowExitOne(ctx, maxToolBytes, args...)
	if err != nil {
		return "", err
	}
	if len(out.data) == 0 {
		return "no matches", nil
	}
	return formatLimitedOutput(out), nil
}

func (r *GitRepository) DiffFile(ctx context.Context, parentSHA, sha, repositoryPath string) (string, error) {
	if err := validateRepositoryPath(repositoryPath); err != nil {
		return "", err
	}
	out, err := r.runBytes(ctx, maxToolBytes, "diff", "--no-ext-diff", "--no-textconv", "--no-color", parentSHA, sha, "--", repositoryPath)
	if err != nil {
		return "", err
	}
	return formatLimitedOutput(out), nil
}

func (r *GitRepository) Log(ctx context.Context, sha, repositoryPath string, maxCount int) (string, error) {
	if maxCount < 1 || maxCount > 20 {
		return "", errors.New("max_count must be between 1 and 20")
	}
	args := []string{"log", "--format=%H %aI %s", "-n", strconv.Itoa(maxCount), sha}
	if repositoryPath != "" {
		if err := validateRepositoryPath(repositoryPath); err != nil {
			return "", err
		}
		args = append(args, "--", repositoryPath)
	}
	out, err := r.runBytes(ctx, maxToolBytes, args...)
	if err != nil {
		return "", err
	}
	return formatLimitedOutput(out), nil
}

func validateRepositoryPath(value string) error {
	if value == "" || strings.ContainsRune(value, '\x00') || strings.Contains(value, "\\") {
		return errors.New("path must be a non-empty repository-relative path")
	}
	clean := path.Clean(value)
	if clean != value || strings.HasPrefix(clean, "/") || clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return errors.New("path must be a clean repository-relative path")
	}
	return nil
}

type numstatEntry struct {
	path   string
	binary bool
}

func parseNumstat(data []byte) ([]numstatEntry, error) {
	data = bytes.TrimSuffix(data, []byte{0})
	if len(data) == 0 {
		return nil, nil
	}
	records := bytes.Split(data, []byte{0})
	entries := make([]numstatEntry, 0, len(records))
	for _, record := range records {
		parts := bytes.SplitN(record, []byte{'\t'}, 3)
		if len(parts) != 3 || len(parts[2]) == 0 {
			return nil, errors.New("unexpected git diff --numstat output")
		}
		entries = append(entries, numstatEntry{
			path:   string(parts[2]),
			binary: string(parts[0]) == "-" && string(parts[1]) == "-",
		})
	}
	return entries, nil
}

type limitedOutput struct {
	data     []byte
	exceeded bool
}

type limitedWriter struct {
	limit    int
	data     bytes.Buffer
	exceeded bool
}

func (w *limitedWriter) Write(p []byte) (int, error) {
	if w.limit <= 0 {
		_, _ = w.data.Write(p)
		return len(p), nil
	}
	remaining := w.limit - w.data.Len()
	if remaining > 0 {
		if remaining > len(p) {
			remaining = len(p)
		}
		_, _ = w.data.Write(p[:remaining])
	}
	if len(p) > remaining {
		w.exceeded = true
	}
	return len(p), nil
}

func (r *GitRepository) run(ctx context.Context, args ...string) (string, error) {
	out, err := r.runBytes(ctx, 0, args...)
	return string(out.data), err
}

func (r *GitRepository) runBytes(ctx context.Context, limit int, args ...string) (limitedOutput, error) {
	return r.runBytesWithExitOne(ctx, limit, false, args...)
}

func (r *GitRepository) runBytesAllowExitOne(ctx context.Context, limit int, args ...string) (limitedOutput, error) {
	return r.runBytesWithExitOne(ctx, limit, true, args...)
}

func (r *GitRepository) runBytesWithExitOne(ctx context.Context, limit int, allowExitOne bool, args ...string) (limitedOutput, error) {
	cmdArgs := []string{"-C", r.WorkTree, "--literal-pathspecs"}
	cmdArgs = append(cmdArgs, args...)
	cmd := exec.CommandContext(ctx, "git", cmdArgs...)
	cmd.Env = append(os.Environ(), "GIT_OPTIONAL_LOCKS=0", "GIT_TERMINAL_PROMPT=0")
	stdout := &limitedWriter{limit: limit}
	var stderr bytes.Buffer
	cmd.Stdout = stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err != nil {
		var exitError *exec.ExitError
		if !(allowExitOne && errors.As(err, &exitError) && exitError.ExitCode() == 1) {
			return limitedOutput{}, commandError("git", cmdArgs, stderr.String(), err)
		}
	}
	return limitedOutput{data: stdout.data.Bytes(), exceeded: stdout.exceeded}, nil
}

func commandError(name string, args []string, stderr string, err error) error {
	detail := strings.TrimSpace(stderr)
	if detail == "" {
		detail = err.Error()
	}
	return fmt.Errorf("%s failed: %s", name, detail)
}

func formatLimitedOutput(out limitedOutput) string {
	if out.exceeded {
		return string(out.data) + "\n[output truncated by air]"
	}
	return string(out.data)
}

func parseObjectIDs(output string) ([]string, error) {
	lines := strings.Fields(output)
	for _, line := range lines {
		if !isHexObjectID(line) {
			return nil, fmt.Errorf("Git returned invalid object ID %q", line)
		}
	}
	return lines, nil
}

func isHexObjectID(value string) bool {
	if len(value) < 40 || len(value) > 64 {
		return false
	}
	for _, c := range value {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}

func shortSHA(sha string) string {
	if len(sha) <= 8 {
		return sha
	}
	return sha[:8]
}
