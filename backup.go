package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"github.com/mattn/go-sqlite3"
)

func runBackup(ctx context.Context, args []string, environment cliEnvironment) error {
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
