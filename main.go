package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	cwd, err := os.Getwd()
	if err != nil {
		fmt.Fprintln(os.Stderr, "air:", err)
		os.Exit(1)
	}
	err = runCLI(ctx, os.Args[1:], cliEnvironment{
		Cwd:    cwd,
		Stdout: os.Stdout,
		Stderr: os.Stderr,
		Getenv: os.Getenv,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "air:", err)
		os.Exit(1)
	}
}
