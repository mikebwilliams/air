package main

import (
	"context"
	"errors"
	"fmt"
)

func runReposeDBCLI(ctx context.Context, args []string, environment cliEnvironment) error {
	flags := newFlagSet("db", environment.Stderr)
	repoPath := flags.String("repo", ".", "dedicated scan checkout")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 1 || flags.Arg(0) != "path" {
		return errors.New("usage: repose db path [--repo DIR]")
	}
	repository, err := DiscoverGitRepository(ctx, reposeAbsolutePath(environment.Cwd, *repoPath))
	if err != nil {
		return err
	}
	databasePath, err := reposeDatabasePath(ctx, repository)
	if err != nil {
		return err
	}
	fmt.Fprintln(environment.Stdout, databasePath)
	return nil
}
