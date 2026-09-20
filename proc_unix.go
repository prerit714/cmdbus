//go:build unix

package main

import (
	"context"
	"os/exec"
	"syscall"
)

// shellCommand returns cmdline as an `sh -c` command that is killed, together
// with everything it spawned, when ctx ends.
func shellCommand(ctx context.Context, cmdline string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "sh", "-c", cmdline)
	// Own process group: the terminal's Ctrl-C reaches only the daemon, and
	// the daemon can kill the whole group.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	return cmd
}
