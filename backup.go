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

	"github.com/mattn/go-sqlite3"
)

func runBackup(ctx context.Context, args []string, environment cliEnvironment) error {
	if len(args) > 0 && args[0] == "import" {
		return runBackupImport(ctx, args[1:], environment)
	}
	flags := newFlagSet("backup", environment.Stderr)
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() > 1 {
		return errors.New("usage: air backup [PATH]")
	}

	repository, store, closeStore, err := openRepositoryStore(ctx, environment.Cwd)
	if err != nil {
		return err
	}
	defer closeStore()

	destination, err := resolveBackupDestination(environment.Cwd, flags.Arg(0), environmentNow(environment))
	if err != nil {
		return err
	}
	source, err := filepath.Abs(repository.DatabasePath())
	if err != nil {
		return fmt.Errorf("resolve AIR database path: %w", err)
	}
	if destination == source {
		return errors.New("backup destination must differ from the AIR database path")
	}
	if err := store.Backup(ctx, destination); err != nil {
		return err
	}
	fmt.Fprintf(environment.Stdout, "Created AIR backup %s\n", destination)
	return nil
}

func runBackupImport(ctx context.Context, args []string, environment cliEnvironment) (returnErr error) {
	flags := newFlagSet("backup import", environment.Stderr)
	force := flags.Bool("force", false, "replace an existing AIR database without prompting")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 1 {
		return errors.New("usage: air backup import [--force] PATH")
	}

	repository, err := DiscoverGitRepository(ctx, environment.Cwd)
	if err != nil {
		return err
	}
	source, err := resolveBackupSource(environment.Cwd, flags.Arg(0))
	if err != nil {
		return err
	}
	destination, err := filepath.Abs(repository.DatabasePath())
	if err != nil {
		return fmt.Errorf("resolve AIR database path: %w", err)
	}
	destination = filepath.Clean(destination)

	sourceInfo, err := os.Stat(source)
	if err != nil {
		return fmt.Errorf("inspect AIR backup %s: %w", source, err)
	}
	if !sourceInfo.Mode().IsRegular() {
		return fmt.Errorf("AIR backup is not a regular file: %s", source)
	}
	destinationInfo, destinationExists, err := regularFileState(destination)
	if err != nil {
		return err
	}
	if source == destination || destinationExists && os.SameFile(sourceInfo, destinationInfo) {
		return errors.New("backup source must differ from the AIR database path")
	}

	sourceStore, err := openReadOnlyAIRStore(ctx, source)
	if err != nil {
		return fmt.Errorf("open AIR backup: %w", err)
	}
	defer func() {
		if closeErr := sourceStore.Close(); returnErr == nil && closeErr != nil {
			returnErr = fmt.Errorf("close AIR backup: %w", closeErr)
		}
	}()
	if err := validateBackupForRepository(ctx, repository, sourceStore); err != nil {
		return fmt.Errorf("validate AIR backup: %w", err)
	}

	if destinationExists && !*force {
		confirmed, err := confirmBackupImport(environment, source, destination)
		if err != nil {
			return err
		}
		if !confirmed {
			fmt.Fprintln(environment.Stdout, "Import cancelled.")
			return nil
		}
	}

	if err := ensureAIRStateDirectory(repository.StateDirectory()); err != nil {
		return err
	}
	lock, err := acquireScanLock(repository.LockPath())
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := lock.Close(); returnErr == nil && closeErr != nil {
			returnErr = fmt.Errorf("release AIR import lock: %w", closeErr)
		}
	}()

	currentInfo, currentExists, err := regularFileState(destination)
	if err != nil {
		return err
	}
	if currentExists != destinationExists || currentExists && !os.SameFile(currentInfo, destinationInfo) {
		return errors.New("AIR database changed during import; retry the command")
	}

	temporary, err := unusedTemporaryPath(repository.StateDirectory(), ".reviews-import-*.sqlite")
	if err != nil {
		return fmt.Errorf("prepare AIR import: %w", err)
	}
	defer removeBackupFiles(temporary)
	if err := sourceStore.Backup(ctx, temporary); err != nil {
		return fmt.Errorf("copy AIR backup: %w", err)
	}
	temporaryStore, err := openReadOnlyAIRStore(ctx, temporary)
	if err != nil {
		return fmt.Errorf("open copied AIR backup: %w", err)
	}
	validationErr := validateBackupForRepository(ctx, repository, temporaryStore)
	closeErr := temporaryStore.Close()
	if validationErr != nil {
		return fmt.Errorf("validate copied AIR backup: %w", validationErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close copied AIR backup: %w", closeErr)
	}
	if err := replaceAIRDatabase(temporary, destination, destinationExists); err != nil {
		return err
	}
	fmt.Fprintf(environment.Stdout, "Imported AIR backup %s to %s\n", source, destination)
	return nil
}

func resolveBackupSource(cwd, requested string) (string, error) {
	if !filepath.IsAbs(requested) {
		requested = filepath.Join(cwd, requested)
	}
	source, err := filepath.Abs(requested)
	if err != nil {
		return "", fmt.Errorf("resolve backup source: %w", err)
	}
	return filepath.Clean(source), nil
}

func openReadOnlyAIRStore(ctx context.Context, databasePath string) (*Store, error) {
	info, err := os.Stat(databasePath)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("database path is not a regular file: %s", databasePath)
	}
	u := &url.URL{Scheme: "file", Path: databasePath}
	query := u.Query()
	query.Set("mode", "ro")
	query.Set("_busy_timeout", "5000")
	query.Set("_foreign_keys", "on")
	query.Set("_query_only", "on")
	u.RawQuery = query.Encode()
	database, err := sql.Open("sqlite3", u.String())
	if err != nil {
		return nil, fmt.Errorf("open SQLite database: %w", err)
	}
	database.SetMaxOpenConns(1)
	if err := database.PingContext(ctx); err != nil {
		_ = database.Close()
		return nil, fmt.Errorf("open SQLite database: %w", err)
	}
	store := &Store{db: database}
	var version int
	if err := database.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version); err != nil {
		_ = store.Close()
		return nil, fmt.Errorf("read schema version: %w", err)
	}
	if version != schemaVersion {
		_ = store.Close()
		return nil, fmt.Errorf("unsupported AIR schema version %d", version)
	}
	return store, nil
}

func validateBackupForRepository(ctx context.Context, repository *GitRepository, store *Store) error {
	if err := store.IntegrityCheck(ctx); err != nil {
		return err
	}
	rows, err := store.db.QueryContext(ctx, `PRAGMA foreign_key_check`)
	if err != nil {
		return fmt.Errorf("check backup foreign keys: %w", err)
	}
	invalidForeignKey := rows.Next()
	rowsErr := rows.Err()
	closeErr := rows.Close()
	if rowsErr != nil {
		return fmt.Errorf("check backup foreign keys: %w", rowsErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close backup foreign-key check: %w", closeErr)
	}
	if invalidForeignKey {
		return errors.New("backup contains invalid foreign-key references")
	}
	startSHA, err := store.Config(ctx, "start_sha")
	if err != nil {
		return err
	}
	onMaster, err := repository.IsOnMasterFirstParent(ctx, startSHA)
	if err != nil {
		return err
	}
	if !onMaster {
		return fmt.Errorf("backup baseline %s is not on the first-parent history of master", shortSHA(startSHA))
	}
	return nil
}

func confirmBackupImport(environment cliEnvironment, source, destination string) (bool, error) {
	if environment.Stdin == nil {
		return false, errors.New("standard input is unavailable; pass --force to import")
	}
	fmt.Fprintf(environment.Stdout, "Replace AIR database %s with backup %s? [y/N] ", destination, source)
	response, err := bufio.NewReader(environment.Stdin).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return false, fmt.Errorf("read import confirmation: %w", err)
	}
	response = strings.ToLower(strings.TrimSpace(response))
	return response == "y" || response == "yes", nil
}

func ensureAIRStateDirectory(directory string) error {
	if err := os.Mkdir(directory, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return fmt.Errorf("create AIR state directory: %w", err)
	}
	info, err := os.Stat(directory)
	if err != nil {
		return fmt.Errorf("inspect AIR state directory: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("AIR state path is not a directory: %s", directory)
	}
	return nil
}

func regularFileState(path string) (os.FileInfo, bool, error) {
	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("inspect AIR database %s: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return nil, false, fmt.Errorf("AIR database is not a regular file: %s", path)
	}
	return info, true, nil
}

func unusedTemporaryPath(directory, pattern string) (string, error) {
	file, err := os.CreateTemp(directory, pattern)
	if err != nil {
		return "", err
	}
	name := file.Name()
	if err := file.Close(); err != nil {
		_ = os.Remove(name)
		return "", err
	}
	if err := os.Remove(name); err != nil {
		return "", err
	}
	return name, nil
}

func replaceAIRDatabase(temporary, destination string, destinationExists bool) error {
	if !destinationExists {
		if err := removeSQLiteSidecars(destination); err != nil {
			return err
		}
		if err := os.Rename(temporary, destination); err != nil {
			return fmt.Errorf("install imported AIR database: %w", err)
		}
		return nil
	}

	rollback, err := unusedTemporaryPath(filepath.Dir(destination), ".reviews-rollback-*.sqlite")
	if err != nil {
		return fmt.Errorf("prepare AIR database replacement: %w", err)
	}
	if err := os.Rename(destination, rollback); err != nil {
		return fmt.Errorf("preserve current AIR database during import: %w", err)
	}
	movedSidecars := make([]string, 0, len(sqliteSidecarSuffixes))
	restore := func() error {
		var restoreErrors []error
		for index := len(movedSidecars) - 1; index >= 0; index-- {
			suffix := movedSidecars[index]
			if err := os.Rename(rollback+suffix, destination+suffix); err != nil {
				restoreErrors = append(restoreErrors, err)
			}
		}
		if err := os.Rename(rollback, destination); err != nil {
			restoreErrors = append(restoreErrors, err)
		}
		return errors.Join(restoreErrors...)
	}
	for _, suffix := range sqliteSidecarSuffixes {
		if _, err := os.Stat(destination + suffix); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			_ = restore()
			return fmt.Errorf("inspect current AIR database sidecar: %w", err)
		}
		if err := os.Rename(destination+suffix, rollback+suffix); err != nil {
			restoreErr := restore()
			return errors.Join(fmt.Errorf("preserve current AIR database sidecar: %w", err), restoreErr)
		}
		movedSidecars = append(movedSidecars, suffix)
	}
	if err := os.Rename(temporary, destination); err != nil {
		restoreErr := restore()
		return errors.Join(fmt.Errorf("install imported AIR database: %w", err), restoreErr)
	}
	if err := removeDatabaseFiles(rollback); err != nil {
		return fmt.Errorf("import succeeded but remove replaced AIR database: %w", err)
	}
	return nil
}

var sqliteSidecarSuffixes = []string{"-journal", "-shm", "-wal"}

func removeSQLiteSidecars(databasePath string) error {
	for _, suffix := range sqliteSidecarSuffixes {
		if err := os.Remove(databasePath + suffix); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove stale AIR database sidecar: %w", err)
		}
	}
	return nil
}

func removeDatabaseFiles(databasePath string) error {
	var removeErrors []error
	if err := os.Remove(databasePath); err != nil && !errors.Is(err, os.ErrNotExist) {
		removeErrors = append(removeErrors, err)
	}
	for _, suffix := range sqliteSidecarSuffixes {
		if err := os.Remove(databasePath + suffix); err != nil && !errors.Is(err, os.ErrNotExist) {
			removeErrors = append(removeErrors, err)
		}
	}
	return errors.Join(removeErrors...)
}

func resolveBackupDestination(cwd, requested string, now time.Time) (string, error) {
	if requested == "" {
		requested = "air-backup-" + now.Format("20060102-150405") + ".sqlite"
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

func nextAvailableBackupPath(cwd, name string) (string, error) {
	extension := filepath.Ext(name)
	stem := name[:len(name)-len(extension)]
	for sequence := 1; ; sequence++ {
		candidate := name
		if sequence > 1 {
			candidate = fmt.Sprintf("%s-%d%s", stem, sequence, extension)
		}
		path := filepath.Join(cwd, candidate)
		if _, err := os.Lstat(path); err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return candidate, nil
			}
			return "", fmt.Errorf("inspect backup destination %s: %w", path, err)
		}
	}
}

func (s *Store) Backup(ctx context.Context, destination string) (err error) {
	file, err := os.OpenFile(destination, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if errors.Is(err, fs.ErrExist) {
			return fmt.Errorf("backup destination already exists: %s", destination)
		}
		return fmt.Errorf("create backup destination %s: %w", destination, err)
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(destination)
		return fmt.Errorf("create backup destination %s: %w", destination, err)
	}

	complete := false
	defer func() {
		if !complete {
			removeBackupFiles(destination)
		}
	}()

	destinationURL := &url.URL{Scheme: "file", Path: destination}
	query := destinationURL.Query()
	query.Set("mode", "rw")
	query.Set("_busy_timeout", "5000")
	destinationURL.RawQuery = query.Encode()
	destinationDB, err := sql.Open("sqlite3", destinationURL.String())
	if err != nil {
		return fmt.Errorf("open backup destination: %w", err)
	}
	destinationDB.SetMaxOpenConns(1)
	destinationClosed := false
	defer func() {
		if !destinationClosed {
			_ = destinationDB.Close()
		}
	}()
	if err := destinationDB.PingContext(ctx); err != nil {
		return fmt.Errorf("open backup destination: %w", err)
	}
	if err := copySQLiteDatabase(ctx, s.db, destinationDB); err != nil {
		return fmt.Errorf("back up AIR database: %w", err)
	}
	if err := (&Store{db: destinationDB}).IntegrityCheck(ctx); err != nil {
		return fmt.Errorf("verify AIR backup: %w", err)
	}
	destinationClosed = true
	if err := destinationDB.Close(); err != nil {
		return fmt.Errorf("close AIR backup: %w", err)
	}
	if err := os.Chmod(destination, 0o600); err != nil {
		return fmt.Errorf("secure AIR backup: %w", err)
	}
	complete = true
	return nil
}

func copySQLiteDatabase(ctx context.Context, source, destination *sql.DB) error {
	sourceConn, err := source.Conn(ctx)
	if err != nil {
		return err
	}
	defer sourceConn.Close()
	destinationConn, err := destination.Conn(ctx)
	if err != nil {
		return err
	}
	defer destinationConn.Close()

	return sourceConn.Raw(func(sourceDriverConn any) error {
		sourceSQLite, ok := sourceDriverConn.(*sqlite3.SQLiteConn)
		if !ok {
			return errors.New("source database is not SQLite")
		}
		return destinationConn.Raw(func(destinationDriverConn any) error {
			destinationSQLite, ok := destinationDriverConn.(*sqlite3.SQLiteConn)
			if !ok {
				return errors.New("backup destination is not SQLite")
			}
			backup, err := destinationSQLite.Backup("main", sourceSQLite, "main")
			if err != nil {
				return err
			}
			closed := false
			defer func() {
				if !closed {
					_ = backup.Close()
				}
			}()
			for {
				done, err := backup.Step(-1)
				if err != nil {
					return err
				}
				if done {
					closed = true
					return backup.Close()
				}
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(10 * time.Millisecond):
				}
			}
		})
	})
}

func removeBackupFiles(destination string) {
	_ = os.Remove(destination)
	_ = os.Remove(destination + "-journal")
	_ = os.Remove(destination + "-shm")
	_ = os.Remove(destination + "-wal")
}
