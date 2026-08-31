package main

import (
	"context"
	"errors"
	"fmt"
)

const airVersion = "0.1"

func runVersion(_ context.Context, args []string, environment cliEnvironment) error {
	positionals, err := parsePositionals("version", args, environment.Stderr)
	if err != nil {
		return err
	}
	if len(positionals) != 0 {
		return errors.New("usage: air version")
	}
	fmt.Fprintf(environment.Stdout, "air %s\n", airVersion)
	return nil
}
