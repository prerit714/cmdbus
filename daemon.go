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
	"syscall"
	"time"
)

const (
	inboxName  = "inbox.jsonl"
	outboxName = "outbox.jsonl"
	logName    = "cmdbus.log"

	statusStarted     = "started"
	statusDone        = "done"
	statusFailed      = "failed"
	statusTimeout     = "timeout"
	statusInterrupted = "interrupted"
)

// Request is one line of inbox.jsonl.
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
	Exit       int    `json:"exit"`
	DurationMS int64  `json:"duration_ms"`
	Output     string `json:"output"`
	Truncated  bool   `json:"truncated"`
	Time       string `json:"time"`
}

type Config struct {
	Dir       string
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

type daemon struct {
	cfg     Config
	log     *slog.Logger
	inbox   string
	outbox  *os.File
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

	last := map[string]string{} // id -> last status
	var order []string
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
			return nil
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
	start := time.Now()
	if err := d.appendOutbox(&Started{ID: req.ID, Status: statusStarted, Time: start.UTC().Format(time.RFC3339)}); err != nil {
		return err
	}
	d.handled[req.ID] = true
	d.log.Info("command_start", "id", req.ID, "cmd", req.Cmd)

	cmdCtx, cancel := context.WithTimeout(ctx, d.cfg.Timeout)
	defer cancel()

	out := &cappedBuffer{max: d.cfg.MaxOutput}
	cmd := exec.CommandContext(cmdCtx, "sh", "-c", req.Cmd)
	cmd.Stdout = out
	cmd.Stderr = out
	// Own process group: the terminal's Ctrl-C reaches only the daemon, and
	// the daemon can kill the command together with everything it spawned.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 2 * time.Second

	runErr := cmd.Run()

	res := Result{ID: req.ID, Exit: -1, Truncated: out.truncated}
	if cmd.ProcessState != nil {
		res.Exit = cmd.ProcessState.ExitCode()
	}
	switch {
	case res.Exit == 0:
		res.Status = statusDone
	case ctx.Err() != nil:
		res.Status = statusInterrupted
	case cmdCtx.Err() != nil:
		res.Status = statusTimeout
	default:
		res.Status = statusFailed
	}
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
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
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
