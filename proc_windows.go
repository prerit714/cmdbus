package main

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"os/exec"
	"strconv"
	"syscall"
	"unicode/utf16"
)

// shellCommand returns cmdline as a PowerShell command that is killed, together
// with everything it spawned, when ctx ends. -EncodedCommand (base64 UTF-16LE)
// avoids every Windows command-line quoting problem.
func shellCommand(ctx context.Context, cmdline string) *exec.Cmd {
	units := utf16.Encode([]rune(cmdline))
	raw := make([]byte, 2*len(units))
	for i, u := range units {
		binary.LittleEndian.PutUint16(raw[2*i:], u)
	}
	cmd := exec.CommandContext(ctx, "powershell.exe", "-NoProfile", "-NonInteractive",
		"-EncodedCommand", base64.StdEncoding.EncodeToString(raw))
	// Own process group: the console's Ctrl-C reaches only the daemon.
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP}
	cmd.Cancel = func() error {
		return exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(cmd.Process.Pid)).Run()
	}
	return cmd
}
