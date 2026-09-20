// This file is the whole daemon.
//
// How it works:
//
//   - The sandbox appends requests to inbox.jsonl; only the daemon writes
//     outbox.jsonl. One writer per file, so nothing is ever locked.
//   - The outbox is the only state. An id that appears in it has been handled
//     and is never run again. On start everything is rebuilt from the files.
//   - Every poll the whole inbox is re-read (it may have been rewritten, not
//     just appended to) and each request whose id is not in the outbox runs,
//     one at a time, in file order.
//   - For each request the daemon appends a "started" line, runs the command,
//     then appends the final line. If the daemon dies in between, the next
//     daemon closes that id as "interrupted" instead of running it twice.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// File names inside Config.Dir.
const (
	inboxName  = "inbox.jsonl"
	outboxName = "outbox.jsonl"
	logName    = "cmdbus.log"
)

// Values of the "status" field in the outbox. Every id gets statusStarted
// followed by exactly one of the others.
const (
	statusStarted     = "started"
	statusDone        = "done"        // exit code 0
	statusFailed      = "failed"      // non-zero exit code, or could not start
	statusTimeout     = "timeout"     // killed after Config.Timeout
	statusInterrupted = "interrupted" // daemon stopped or died mid-command
)

// Request is one line of inbox.jsonl. Both fields are required.
type Request struct {
	ID  string `json:"id"`
	Cmd string `json:"cmd"`
}

// Started is the outbox line written before a command runs. An ID that has
// this line is never run again, whatever happens next.
type Started struct {
	ID     string `json:"id"`
	Status string `json:"status"`
	Time   string `json:"time"`
}

// Result is the final outbox line for an ID.
type Result struct {
	ID         string `json:"id"`
	Status     string `json:"status"`
	Exit       int    `json:"exit"` // -1 when there is no exit code (killed, never started)
	DurationMS int64  `json:"duration_ms"`
	Output     string `json:"output"`    // stdout and stderr combined, valid UTF-8
	Truncated  bool   `json:"truncated"` // Output hit Config.MaxOutput
	Time       string `json:"time"`      // RFC 3339, UTC; when the line was written
}

// Config is everything Run needs. The command line only exposes Dir and
// Timeout; Poll and MaxOutput are fields so that tests can shrink them.
type Config struct {
	Dir       string        // holds the inbox, the outbox and the log
	Timeout   time.Duration // per command
	Poll      time.Duration // inbox check interval
	MaxOutput int           // bytes of output kept per command
	Log       *slog.Logger
}

// newLogger returns a JSON logger that writes to w and appends to
// <dir>/cmdbus.log, so a running daemon can be followed with tail -f.
func newLogger(dir string, w io.Writer) (*slog.Logger, io.Closer, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, nil, err
	}
	f, err := os.OpenFile(filepath.Join(dir, logName), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, nil, err
	}
	return slog.New(slog.NewJSONHandler(io.MultiWriter(w, f), nil)), f, nil
}

// daemon is the state of one Run call. It is used from a single goroutine.
type daemon struct {
	cfg     Config
	log     *slog.Logger
	inbox   string          // path; re-read on every poll
	outbox  *os.File        // opened once with O_APPEND
	handled map[string]bool // IDs present in the outbox
	warned  map[string]bool // bad inbox lines already logged
}

// Run watches the inbox and executes new requests until ctx is cancelled.
// It returns nil on cancellation and an error only if the daemon cannot work.
func Run(ctx context.Context, cfg Config) error {
	if err := os.MkdirAll(cfg.Dir, 0o755); err != nil {
		return err
	}
	d := &daemon{
		cfg:     cfg,
		log:     cfg.Log,
		inbox:   filepath.Join(cfg.Dir, inboxName),
		handled: map[string]bool{},
		warned:  map[string]bool{},
	}
	// Create the inbox if missing so the sandbox has a file to append to.
	f, err := os.OpenFile(d.inbox, os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	f.Close()

	if err := d.openOutbox(); err != nil {
		return err
	}
	defer d.outbox.Close()

	cwd, _ := os.Getwd()
	d.log.Info("daemon_start", "dir", cfg.Dir, "cwd", cwd, "timeout", cfg.Timeout.String(), "pid", os.Getpid())

	// Process first, then wait: requests already in the inbox run at once.
	ticker := time.NewTicker(cfg.Poll)
	defer ticker.Stop()
	for {
		if err := d.processInbox(ctx); err != nil {
			d.log.Error("error", "op", "process_inbox", "err", err.Error())
			return err
		}
		select {
		case <-ctx.Done():
			d.log.Info("daemon_stop", "reason", "signal")
			return nil
		case <-ticker.C:
		}
	}
}

// openOutbox opens the outbox for appending, loads the handled IDs from it and
// closes any ID that was left at "started" by a previous daemon.
func (d *daemon) openOutbox() error {
	path := filepath.Join(d.cfg.Dir, outboxName)
	data, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	d.outbox, err = os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	// A previous daemon may have died mid-write; keep the next line valid.
	if len(data) > 0 && data[len(data)-1] != '\n' {
		if _, err := d.outbox.Write([]byte("\n")); err != nil {
			return err
		}
	}

	// Only id and status matter here, and Started has both. Lines that do
	// not parse (a torn write) are ignored.
	last := map[string]string{} // id -> status of its last line
	var order []string          // ids in first-seen order, for stable recovery output
	for _, line := range bytes.Split(data, []byte("\n")) {
		var s Started
		if json.Unmarshal(line, &s) != nil || s.ID == "" {
			continue
		}
		if _, seen := last[s.ID]; !seen {
			order = append(order, s.ID)
		}
		last[s.ID] = s.Status
	}
	for _, id := range order {
		d.handled[id] = true
		if last[id] != statusStarted {
			continue
		}
		res := Result{ID: id, Status: statusInterrupted, Exit: -1, Output: "cmdbus: daemon stopped while this command was running; it was not re-run\n"}
		if err := d.appendOutbox(&res); err != nil {
			return err
		}
		d.log.Warn("recovered_interrupted", "id", id)
	}
	return nil
}

// processInbox re-reads the whole inbox and runs every request not yet in the
// outbox, in file order. The inbox may be appended to or rewritten freely.
func (d *daemon) processInbox(ctx context.Context) error {
	data, err := os.ReadFile(d.inbox)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	lines := bytes.Split(data, []byte("\n"))
	// The last element is either empty or a line still being written.
	lines = lines[:len(lines)-1]

	for i, line := range lines {
		if ctx.Err() != nil {
			return nil // shutting down: start nothing new
		}
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		var req Request
		reason := ""
		if err := json.Unmarshal(line, &req); err != nil {
			reason = err.Error()
		} else if req.ID == "" || req.Cmd == "" {
			reason = `"id" and "cmd" are required`
		}
		if reason != "" {
			// Log a bad line once, not on every poll.
			if !d.warned[string(line)] {
				d.warned[string(line)] = true
				d.log.Warn("inbox_bad_line", "line", i+1, "reason", reason)
			}
			continue
		}
		if d.handled[req.ID] {
			continue
		}
		if err := d.execute(ctx, req); err != nil {
			return err
		}
	}
	return nil
}

// execute runs one request. The returned error is only about the outbox:
// if results cannot be recorded, the daemon must stop.
func (d *daemon) execute(ctx context.Context, req Request) error {
	// Record "started" before running: from here on this id is never run
	// again, even if the daemon is killed before the command finishes.
	start := time.Now()
	if err := d.appendOutbox(&Started{ID: req.ID, Status: statusStarted, Time: start.UTC().Format(time.RFC3339)}); err != nil {
		return err
	}
	d.handled[req.ID] = true
	d.log.Info("command_start", "id", req.ID, "cmd", req.Cmd)

	cmdCtx, cancel := context.WithTimeout(ctx, d.cfg.Timeout)
	defer cancel()

	out := &cappedBuffer{max: d.cfg.MaxOutput}
	cmd := shellCommand(cmdCtx, req.Cmd) // proc_unix.go / proc_windows.go
	cmd.Stdout = out
	cmd.Stderr = out
	// If the command leaves a background child holding its output pipe, do
	// not wait for that child: give up 2s after the shell itself has exited.
	cmd.WaitDelay = 2 * time.Second

	runErr := cmd.Run()

	res := Result{ID: req.ID, Exit: -1, Truncated: out.truncated}
	if cmd.ProcessState != nil {
		res.Exit = cmd.ProcessState.ExitCode()
	}
	// A clean exit wins even if a timeout or Ctrl-C raced with it: the
	// command did complete. Otherwise the daemon's own shutdown explains a
	// kill before the per-command timeout does.
	switch {
	case res.Exit == 0:
		res.Status = statusDone
	case ctx.Err() != nil:
		res.Status, res.Exit = statusInterrupted, -1
	case cmdCtx.Err() != nil:
		res.Status, res.Exit = statusTimeout, -1
	default:
		res.Status = statusFailed
	}
	// Errors other than a plain non-zero exit (shell missing, WaitDelay
	// expired, ...) would otherwise be invisible to the requester.
	var exitErr *exec.ExitError
	if runErr != nil && !errors.As(runErr, &exitErr) {
		fmt.Fprintf(&out.buf, "cmdbus: %v\n", runErr)
	}
	res.Output = strings.ToValidUTF8(out.buf.String(), "�")
	res.DurationMS = time.Since(start).Milliseconds()

	if err := d.appendOutbox(&res); err != nil {
		return err
	}
	d.log.Info("command_done", "id", res.ID, "status", res.Status, "exit", res.Exit,
		"duration_ms", res.DurationMS, "output_bytes", len(res.Output), "truncated", res.Truncated)
	return nil
}

// appendOutbox writes v as one JSON line with a single write call.
func (d *daemon) appendOutbox(v any) error {
	if r, ok := v.(*Result); ok && r.Time == "" {
		r.Time = time.Now().UTC().Format(time.RFC3339)
	}
	// Encode into memory first so the file sees one write of one full line;
	// a reader never finds half a line unless the daemon is killed mid-write.
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false) // keep <, > and & readable in command output
	if err := enc.Encode(v); err != nil {
		return err
	}
	if _, err := d.outbox.Write(buf.Bytes()); err != nil {
		d.log.Error("error", "op", "append_outbox", "err", err.Error())
		return err
	}
	return nil
}

// cappedBuffer keeps the first max bytes written to it and drops the rest.
type cappedBuffer struct {
	buf       bytes.Buffer
	max       int
	truncated bool
}

// Write always reports success: returning a short write or an error would make
// os/exec stop draining the pipe and the command could block on a full pipe.
func (c *cappedBuffer) Write(p []byte) (int, error) {
	room := c.max - c.buf.Len()
	if room < len(p) {
		c.truncated = true
		if room < 0 {
			room = 0
		}
		c.buf.Write(p[:room])
		return len(p), nil
	}
	return c.buf.Write(p)
}
