package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
)

func runHint(ctx context.Context, args []string, environment cliEnvironment) error {
	if len(args) == 0 {
		return errors.New("usage: air hint <add|list|remove> ...")
	}
	command := args[0]
	if command != "add" && command != "list" && command != "remove" {
		return fmt.Errorf("unknown hint command %q; expected add, list, or remove", command)
	}
	flags := newFlagSet("hint "+command, environment.Stderr)
	var version string
	if command == "list" {
		flags.StringVar(&version, "prompt", "", "show the hint snapshot for a recorded prompt identity")
	}
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	wantArgs := 1
	if command == "list" {
		wantArgs = 0
	}
	if flags.NArg() != wantArgs {
		return fmt.Errorf("usage: air hint %s %s", command, map[string]string{
			"add": "TEXT", "list": "[--prompt IDENTITY]", "remove": "HINT_ID",
		}[command])
	}
	var id int64
	if command == "remove" {
		var err error
		id, err = strconv.ParseInt(flags.Arg(0), 10, 64)
		if err != nil || id <= 0 {
			return errors.New("hint ID must be a positive integer")
		}
	}
	_, store, closeStore, err := openRepositoryStore(ctx, environment.Cwd)
	if err != nil {
		return err
	}
	defer closeStore()
	switch command {
	case "add":
		id, err := store.AddHint(ctx, flags.Arg(0))
		if err != nil {
			return err
		}
		fmt.Fprintf(environment.Stdout, "Added hint #%d.\n", id)
	case "remove":
		if err := store.RemoveHint(ctx, id); err != nil {
			return err
		}
		fmt.Fprintf(environment.Stdout, "Removed hint #%d; recorded review snapshots are retained.\n", id)
	case "list":
		var hints []ReviewHint
		if flags.Changed("prompt") {
			if !strings.HasPrefix(version, "hints:sha256:") {
				return errors.New("--prompt requires a recorded hints:sha256: identity")
			}
			hints, err = store.HintsForPrompt(ctx, version)
		} else {
			hints, err = store.Hints(ctx)
		}
		if err != nil {
			return err
		}
		if len(hints) == 0 {
			fmt.Fprintln(environment.Stdout, "No active hints.")
		} else {
			printHints(environment.Stdout, hints)
		}
	}
	return nil
}

func printHints(output io.Writer, hints []ReviewHint) {
	for _, hint := range hints {
		label := "one-off"
		if hint.ID != 0 {
			label = fmt.Sprintf("#%d", hint.ID)
		}
		fmt.Fprintf(output, "  %s  %s\n", label, strings.ReplaceAll(hint.Text, "\n", "\n      "))
	}
}

func printReviewHints(output io.Writer, hints []ReviewHint) {
	if len(hints) != 0 {
		fmt.Fprintln(output, "\nHints used:")
		printHints(output, hints)
	}
}
