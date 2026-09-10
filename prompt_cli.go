package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

func runPrompt(ctx context.Context, args []string, environment cliEnvironment) error {
	if len(args) == 0 {
		return errors.New("usage: air prompt <list|show|set|reset> ...")
	}
	switch args[0] {
	case "list":
		return runPromptList(ctx, args[1:], environment)
	case "show":
		return runPromptShow(ctx, args[1:], environment)
	case "set":
		return runPromptSet(ctx, args[1:], environment)
	case "reset":
		return runPromptReset(ctx, args[1:], environment)
	default:
		return fmt.Errorf("unknown prompt command %q; expected list, show, set, or reset", args[0])
	}
}

func runPromptList(ctx context.Context, args []string, environment cliEnvironment) error {
	positionals, err := parsePositionals("prompt list", args, environment.Stderr)
	if err != nil {
		return err
	}
	if len(positionals) != 0 {
		return errors.New("usage: air prompt list")
	}
	_, store, closeStore, err := openRepositoryStore(ctx, environment.Cwd)
	if err != nil {
		return err
	}
	defer closeStore()

	fmt.Fprintln(environment.Stdout, "KIND     SOURCE    IDENTITY")
	for _, spec := range reviewerPromptSpecs() {
		prompt, err := resolveReviewerPrompt(ctx, store, spec.Kind)
		if err != nil {
			return err
		}
		fmt.Fprintf(environment.Stdout, "%-8s %-9s %s\n",
			prompt.Kind, prompt.Source, prompt.PromptVersion)
	}
	return nil
}

func runPromptShow(ctx context.Context, args []string, environment cliEnvironment) error {
	flags := newFlagSet("prompt show", environment.Stderr)
	full := flags.Bool("full", false, "include active hints and AIR's fixed protocol and response contract")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 1 {
		return errors.New("usage: air prompt show [--full] <review|recheck>")
	}
	_, store, closeStore, err := openRepositoryStore(ctx, environment.Cwd)
	if err != nil {
		return err
	}
	defer closeStore()
	prompt, err := resolveReviewerPrompt(ctx, store, flags.Arg(0))
	if err != nil {
		return err
	}
	value := prompt.Instructions
	if *full {
		value = prompt.Static
	}
	fmt.Fprint(environment.Stdout, value)
	if !strings.HasSuffix(value, "\n") {
		fmt.Fprintln(environment.Stdout)
	}
	return nil
}

func runPromptSet(ctx context.Context, args []string, environment cliEnvironment) error {
	flags := newFlagSet("prompt set", environment.Stderr)
	filename := flags.String("file", "", "read instructions from this file")
	fromStdin := flags.Bool("stdin", false, "read instructions from standard input")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 1 || ((*filename == "") == !*fromStdin) {
		return errors.New("usage: air prompt set (--file <path>|--stdin) <review|recheck>")
	}
	spec, err := reviewerPromptSpec(flags.Arg(0))
	if err != nil {
		return err
	}
	var contents []byte
	if *fromStdin {
		if environment.Stdin == nil {
			return errors.New("standard input is unavailable")
		}
		contents, err = readPromptContents(environment.Stdin)
		if err != nil {
			return fmt.Errorf("read prompt from standard input: %w", err)
		}
	} else {
		path := *filename
		if !filepath.IsAbs(path) {
			path = filepath.Join(environment.Cwd, path)
		}
		file, openErr := os.Open(path)
		if openErr != nil {
			return fmt.Errorf("open prompt file: %w", openErr)
		}
		contents, err = readPromptContents(file)
		closeErr := file.Close()
		if err != nil {
			return fmt.Errorf("read prompt file: %w", err)
		}
		if closeErr != nil {
			return fmt.Errorf("close prompt file: %w", closeErr)
		}
	}
	instructions, err := validatePromptInstructions(string(contents))
	if err != nil {
		return fmt.Errorf("invalid prompt: %w", err)
	}
	_, store, closeStore, err := openRepositoryStore(ctx, environment.Cwd)
	if err != nil {
		return err
	}
	defer closeStore()
	if err := store.SetConfig(ctx, spec.ConfigKey, instructions); err != nil {
		return err
	}
	custom, err := resolveReviewerPrompt(ctx, store, spec.Kind)
	if err != nil {
		return err
	}
	fmt.Fprintf(environment.Stdout, "Set %s prompt (%s)\n",
		custom.Kind, custom.PromptVersion)
	return nil
}

func runPromptReset(ctx context.Context, args []string, environment cliEnvironment) error {
	flags := newFlagSet("prompt reset", environment.Stderr)
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 1 {
		return errors.New("usage: air prompt reset <review|recheck>")
	}
	spec, err := reviewerPromptSpec(flags.Arg(0))
	if err != nil {
		return err
	}
	_, store, closeStore, err := openRepositoryStore(ctx, environment.Cwd)
	if err != nil {
		return err
	}
	defer closeStore()
	removed, err := store.UnsetConfig(ctx, spec.ConfigKey)
	if err != nil {
		return err
	}
	if removed {
		fmt.Fprintf(environment.Stdout, "Reset %s prompt to built-in version %s\n",
			spec.Kind, spec.BuiltinVersion)
	} else {
		fmt.Fprintf(environment.Stdout, "%s prompt already uses built-in version %s\n",
			spec.Kind, spec.BuiltinVersion)
	}
	return nil
}

func readPromptContents(reader io.Reader) ([]byte, error) {
	contents, err := io.ReadAll(io.LimitReader(reader, maximumPromptBytes+1))
	if err != nil {
		return nil, err
	}
	if len(contents) > maximumPromptBytes {
		return nil, fmt.Errorf("prompt exceeds %d KiB", maximumPromptBytes/1024)
	}
	return contents, nil
}
