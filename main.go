package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	cwd, err := os.Getwd()
	if err != nil {
		fmt.Fprintln(os.Stderr, "repose:", err)
		os.Exit(1)
	}
	err = runReposeCLI(ctx, os.Args[1:], cliEnvironment{
		Cwd:    cwd,
		Stdin:  os.Stdin,
		Stdout: os.Stdout,
		Stderr: os.Stderr,
		Getenv: os.Getenv,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "repose:", err)
		os.Exit(1)
	}
}
