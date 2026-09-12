//go:build !linux && !darwin && !freebsd && !netbsd && !openbsd && !dragonfly

package main

import (
	"os/exec"
	"time"
)

func configureAuditProcess(command *exec.Cmd) { command.WaitDelay = 2 * time.Second }
