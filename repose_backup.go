package main

import (
	"bufio"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func runReposeBackupCLI(ctx context.Context, args []string, environment cliEnvironment) error {
	if len(args) > 0 && args[0] == "import" {
		return runReposeBackupImportCLI(ctx, args[1:], environment)
	}
	flags := newFlagSet("backup", environment.Stderr)
	repo := flags.String("repo", ".", "scan checkout")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() > 1 {
		return errors.New("usage: repose backup [PATH] [--repo DIR]")
	}
	repository, err := DiscoverGitRepository(ctx, reposeAbsolutePath(environment.Cwd, *repo))
	if err != nil {
		return err
	}
	database, err := reposeDatabasePath(ctx, repository)
	if err != nil {
		return err
	}
	reader, err := openInventoryReadOnly(ctx, database)
	if err != nil {
		return err
	}
	defer reader.Close()
	destination, err := resolveReposeBackupDestination(environment.Cwd, flags.Arg(0), environmentNow(environment))
	if err != nil {
		return err
	}
	if destination == filepath.Clean(database) {
		return errors.New("backup destination must differ from the Repose database path")
	}
	if err := reader.Backup(ctx, destination); err != nil {
		return err
	}
	if err := validateSavedReposeBackup(ctx, repository, destination); err != nil {
		removeBackupFiles(destination)
		return fmt.Errorf("validate Repose backup: %w", err)
	}
	fmt.Fprintf(environment.Stdout, "Created Repose backup %s\n", destination)
	return nil
}

func runReposeBackupImportCLI(ctx context.Context, args []string, environment cliEnvironment) (returnErr error) {
	flags := newFlagSet("backup import", environment.Stderr)
	repo := flags.String("repo", ".", "scan checkout")
	force := flags.Bool("force", false, "replace an existing Repose database without prompting")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 1 {
		return errors.New("usage: repose backup import [--force] PATH [--repo DIR]")
	}
	repository, err := DiscoverGitRepository(ctx, reposeAbsolutePath(environment.Cwd, *repo))
	if err != nil {
		return err
	}
	source, err := resolveBackupSource(environment.Cwd, flags.Arg(0))
	if err != nil {
		return err
	}
	destination, err := reposeDatabasePath(ctx, repository)
	if err != nil {
		return err
	}
	destination = filepath.Clean(destination)
	sourceInfo, err := os.Stat(source)
	if err != nil {
		return fmt.Errorf("inspect Repose backup %s: %w", source, err)
	}
	if !sourceInfo.Mode().IsRegular() {
		return fmt.Errorf("Repose backup is not a regular file: %s", source)
	}
	destinationInfo, destinationExists, err := reposeRegularFileState(destination)
	if err != nil {
		return err
	}
	if source == destination || destinationExists && os.SameFile(sourceInfo, destinationInfo) {
		return errors.New("backup source must differ from the Repose database path")
	}
	sourceStore, err := openReposeBackupReadOnly(ctx, source)
	if err != nil {
		return fmt.Errorf("open Repose backup: %w", err)
	}
	defer func() {
		if closeErr := sourceStore.Close(); returnErr == nil && closeErr != nil {
			returnErr = fmt.Errorf("close Repose backup: %w", closeErr)
		}
	}()
	if err := validateReposeBackupForRepository(ctx, repository, sourceStore); err != nil {
		return fmt.Errorf("validate Repose backup: %w", err)
	}
	if destinationExists && !*force {
		confirmed, err := confirmReposeBackupImport(environment, source, destination)
		if err != nil {
			return err
		}
		if !confirmed {
			fmt.Fprintln(environment.Stdout, "Import cancelled.")
			return nil
		}
	}
	stateDirectory := filepath.Dir(destination)
	if err := ensureReposeStateDirectory(stateDirectory); err != nil {
		return err
	}
	scanLock, err := acquireScanLock(filepath.Join(stateDirectory, "scan.lock"))
	if err != nil {
		return fmt.Errorf("acquire Repose scan lock: %w", err)
	}
	defer func() {
		if closeErr := scanLock.Close(); returnErr == nil && closeErr != nil {
			returnErr = fmt.Errorf("release Repose scan lock: %w", closeErr)
		}
	}()
	indexLock, err := acquireScanLock(filepath.Join(stateDirectory, "index.lock"))
	if err != nil {
		return fmt.Errorf("acquire Repose index lock: %w", err)
	}
	defer func() {
		if closeErr := indexLock.Close(); returnErr == nil && closeErr != nil {
			returnErr = fmt.Errorf("release Repose index lock: %w", closeErr)
		}
	}()
	currentInfo, currentExists, err := reposeRegularFileState(destination)
	if err != nil {
		return err
	}
	if currentExists != destinationExists || currentExists && !os.SameFile(currentInfo, destinationInfo) {
		return errors.New("Repose database changed during import; retry the command")
	}
	temporary, err := unusedTemporaryPath(stateDirectory, ".repose-import-*.sqlite")
	if err != nil {
		return fmt.Errorf("prepare Repose import: %w", err)
	}
	defer removeBackupFiles(temporary)
	if err := sourceStore.Backup(ctx, temporary); err != nil {
		return fmt.Errorf("copy Repose backup: %w", err)
	}
	temporaryStore, err := openReposeBackupReadOnly(ctx, temporary)
	if err != nil {
		return fmt.Errorf("open copied Repose backup: %w", err)
	}
	validationErr := validateReposeBackupForRepository(ctx, repository, temporaryStore)
	closeErr := temporaryStore.Close()
	if validationErr != nil {
		return fmt.Errorf("validate copied Repose backup: %w", validationErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close copied Repose backup: %w", closeErr)
	}
	if err := replaceSQLiteDatabase(temporary, destination, destinationExists, "Repose", ".repose-rollback-*.sqlite"); err != nil {
		return err
	}
	fmt.Fprintf(environment.Stdout, "Imported Repose backup %s to %s\n", source, destination)
	return nil
}

func validateSavedReposeBackup(ctx context.Context, repository *GitRepository, filename string) error {
	store, err := openReposeBackupReadOnly(ctx, filename)
	if err != nil {
		return err
	}
	defer store.Close()
	return validateReposeBackupForRepository(ctx, repository, store)
}

func openReposeBackupReadOnly(ctx context.Context, filename string) (*inventoryStore, error) {
	info, err := os.Stat(filename)
	if err != nil {
		return nil, fmt.Errorf("open Repose backup: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("Repose backup is not a regular file: %s", filename)
	}
	u := &url.URL{Scheme: "file", Path: filename}
	query := u.Query()
	query.Set("mode", "ro")
	query.Set("immutable", "1")
	query.Set("_busy_timeout", "5000")
	query.Set("_foreign_keys", "on")
	query.Set("_query_only", "on")
	u.RawQuery = query.Encode()
	database, err := sql.Open("sqlite3", u.String())
	if err != nil {
		return nil, fmt.Errorf("open Repose backup: %w", err)
	}
	database.SetMaxOpenConns(1)
	if err := database.PingContext(ctx); err != nil {
		database.Close()
		return nil, fmt.Errorf("open Repose backup: %w", err)
	}
	store := &Store{db: database}
	var version, application int
	if err = database.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err == nil {
		err = database.QueryRowContext(ctx, "PRAGMA application_id").Scan(&application)
	}
	if err == nil && ((version < 1 || version > reposeCurrentSchemaVersion) || application != 1380994899) {
		err = fmt.Errorf("unsupported Repose database (application %d, version %d)", application, version)
	}
	if err != nil {
		store.Close()
		return nil, err
	}
	return &inventoryStore{Store: store, version: version}, nil
}

func validateReposeBackupForRepository(ctx context.Context, repository *GitRepository, store *inventoryStore) error {
	if err := store.IntegrityCheck(ctx); err != nil {
		return err
	}
	if err := validateBackupForeignKeys(ctx, store.Store); err != nil {
		return err
	}
	inventories, err := store.ListInventories(ctx)
	if err != nil {
		return err
	}
	if len(inventories) == 0 {
		return errors.New("backup contains no inventories")
	}
	for _, item := range inventories {
		record, err := store.Inventory(ctx, item.ID)
		if err != nil {
			return fmt.Errorf("validate inventory %s: %w", shortSHA(item.ID), err)
		}
		if item.SnapshotSHA != record.Inventory.SnapshotSHA {
			return fmt.Errorf("inventory %s snapshot column does not match its document", shortSHA(item.ID))
		}
		resolved, err := repository.ResolveCommit(ctx, item.SnapshotSHA)
		if err != nil || resolved != item.SnapshotSHA {
			return fmt.Errorf("inventory %s snapshot %s is unavailable in the target repository", shortSHA(item.ID), shortSHA(item.SnapshotSHA))
		}
	}
	return nil
}

func resolveReposeBackupDestination(cwd, requested string, now time.Time) (string, error) {
	if requested == "" {
		requested = "repose-backup-" + now.Format("20060102-150405") + ".sqlite"
		var err error
		requested, err = nextAvailableBackupPath(cwd, requested)
		if err != nil {
			return "", err
		}
	}
	if !filepath.IsAbs(requested) {
		requested = filepath.Join(cwd, requested)
	}
	destination, err := filepath.Abs(requested)
	if err != nil {
		return "", fmt.Errorf("resolve backup destination: %w", err)
	}
	return filepath.Clean(destination), nil
}

func reposeRegularFileState(path string) (os.FileInfo, bool, error) {
	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("inspect Repose database %s: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return nil, false, fmt.Errorf("Repose database is not a regular file: %s", path)
	}
	return info, true, nil
}

func ensureReposeStateDirectory(directory string) error {
	if err := os.Mkdir(directory, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
		return fmt.Errorf("create Repose state directory: %w", err)
	}
	info, err := os.Stat(directory)
	if err != nil {
		return fmt.Errorf("inspect Repose state directory: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("Repose state path is not a directory: %s", directory)
	}
	return nil
}

func confirmReposeBackupImport(environment cliEnvironment, source, destination string) (bool, error) {
	if environment.Stdin == nil {
		return false, errors.New("standard input is unavailable; pass --force to import")
	}
	fmt.Fprintf(environment.Stdout, "Replace Repose database %s with backup %s? [y/N] ", destination, source)
	response, err := bufio.NewReader(environment.Stdin).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return false, fmt.Errorf("read import confirmation: %w", err)
	}
	response = strings.ToLower(strings.TrimSpace(response))
	return response == "y" || response == "yes", nil
}
