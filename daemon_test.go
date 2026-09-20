package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// bus is a temp bus directory with helpers to drive a daemon against it.
type bus struct {
	t   *testing.T
	dir string
	cfg Config
}

func newBus(t *testing.T) *bus {
	t.Helper()
	dir := t.TempDir()
	return &bus{t: t, dir: dir, cfg: Config{
		Dir:       dir,
		Timeout:   10 * time.Second,
		Poll:      10 * time.Millisecond,
		MaxOutput: 1 << 20,
		Log:       slog.New(slog.NewJSONHandler(io.Discard, nil)),
	}}
}

// start runs the daemon and returns a function that stops it (like Ctrl-C)
// and waits for Run to return.
func (b *bus) start() (stop func()) {
	b.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Run(ctx, b.cfg) }()
	stopped := false
	stop = func() {
		b.t.Helper()
		if stopped {
			return
		}
		stopped = true
		cancel()
		select {
		case err := <-done:
			if err != nil {
				b.t.Errorf("Run returned error: %v", err)
			}
		case <-time.After(5 * time.Second):
			b.t.Fatal("Run did not return within 5s of cancellation")
		}
	}
	b.t.Cleanup(stop)
	return stop
}

func (b *bus) path(name string) string { return filepath.Join(b.dir, name) }

func (b *bus) appendInbox(s string) {
	b.t.Helper()
	f, err := os.OpenFile(b.path(inboxName), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		b.t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(s); err != nil {
		b.t.Fatal(err)
	}
}

func (b *bus) request(id, cmd string) {
	b.t.Helper()
	line, _ := json.Marshal(Request{ID: id, Cmd: cmd})
	b.appendInbox(string(line) + "\n")
}

// outbox returns every outbox line, in order. Every line must be valid JSON.
func (b *bus) outbox() []Result {
	b.t.Helper()
	data, err := os.ReadFile(b.path(outboxName))
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		b.t.Fatal(err)
	}
	var all []Result
	for _, line := range bytes.Split(bytes.TrimSuffix(data, []byte("\n")), []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		var r Result
		if err := json.Unmarshal(line, &r); err != nil {
			// The only tolerated junk is a torn line from a crashed daemon.
			if !strings.HasPrefix(string(line), `{"id":"torn"`) {
				b.t.Fatalf("outbox line is not JSON: %q", line)
			}
			continue
		}
		all = append(all, r)
	}
	return all
}

func (b *bus) lines(id string) []Result {
	var rs []Result
	for _, r := range b.outbox() {
		if r.ID == id {
			rs = append(rs, r)
		}
	}
	return rs
}

// wait blocks until id has a line with the wanted status ("" = any final one).
func (b *bus) wait(id, status string) Result {
	b.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		for _, r := range b.lines(id) {
			if r.Status == status || (status == "" && r.Status != statusStarted) {
				return r
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	b.t.Fatalf("timed out waiting for %q status %q; outbox: %+v", id, status, b.outbox())
	return Result{}
}

// settle gives the daemon several poll cycles to do anything it wrongly might.
func (b *bus) settle() { time.Sleep(15 * b.cfg.Poll) }

func (b *bus) countLines(file string) int {
	data, _ := os.ReadFile(file)
	return bytes.Count(data, []byte("\n"))
}

func TestRequestProducesExactlyOneResult(t *testing.T) {
	b := newBus(t)
	b.start()
	b.request("a", "echo hello; echo oops >&2")
	r := b.wait("a", "")
	if r.Status != statusDone || r.Exit != 0 || r.Output != "hello\noops\n" || r.Truncated {
		t.Fatalf("unexpected result: %+v", r)
	}
	if _, err := time.Parse(time.RFC3339, r.Time); err != nil {
		t.Fatalf("bad time %q", r.Time)
	}
	b.settle()
	ls := b.lines("a")
	if len(ls) != 2 || ls[0].Status != statusStarted || ls[1].Status != statusDone {
		t.Fatalf("want started+done, got %+v", ls)
	}
}

func TestNonZeroExitIsFailed(t *testing.T) {
	b := newBus(t)
	b.start()
	b.request("a", "echo bye; exit 3")
	r := b.wait("a", "")
	if r.Status != statusFailed || r.Exit != 3 || r.Output != "bye\n" {
		t.Fatalf("unexpected result: %+v", r)
	}
}

func TestOrderPreserved(t *testing.T) {
	b := newBus(t)
	var sb strings.Builder
	for i := 0; i < 5; i++ {
		line, _ := json.Marshal(Request{ID: fmt.Sprint("r", i), Cmd: fmt.Sprint("echo ", i)})
		sb.Write(line)
		sb.WriteByte('\n')
	}
	b.appendInbox(sb.String())
	b.start()
	b.wait("r4", "")
	var got []string
	for _, r := range b.outbox() {
		got = append(got, r.ID+":"+r.Status)
	}
	want := "r0:started r0:done r1:started r1:done r2:started r2:done r3:started r3:done r4:started r4:done"
	if strings.Join(got, " ") != want {
		t.Fatalf("got  %v\nwant %v", strings.Join(got, " "), want)
	}
}

func TestAwkwardOutputRoundTrips(t *testing.T) {
	b := newBus(t)
	b.start()
	b.request("a", `printf '%s\n' '"quoted"' '<b>&</b>' '`+"```"+`' 'tab	here' '{"id":"fake","status":"done"}'; printf 'bad:\377\n'`)
	r := b.wait("a", "")
	want := "\"quoted\"\n<b>&</b>\n```\ntab\there\n{\"id\":\"fake\",\"status\":\"done\"}\nbad:�\n"
	if r.Output != want {
		t.Fatalf("got  %q\nwant %q", r.Output, want)
	}
	if len(b.lines("fake")) != 0 {
		t.Fatal("command output leaked into the outbox as its own line")
	}
	raw, _ := os.ReadFile(b.path(outboxName))
	if !bytes.Contains(raw, []byte("<b>&</b>")) {
		t.Fatal("output should not be HTML-escaped")
	}
}

func TestUnterminatedLineWaits(t *testing.T) {
	b := newBus(t)
	b.start()
	b.appendInbox(`{"id":"a","cmd":"echo hi"}`)
	b.settle()
	if n := len(b.lines("a")); n != 0 {
		t.Fatalf("ran a request whose line was not finished: %d lines", n)
	}
	b.appendInbox("\n")
	if r := b.wait("a", ""); r.Output != "hi\n" {
		t.Fatalf("unexpected result: %+v", r)
	}
}

func TestBadLinesAreSkipped(t *testing.T) {
	b := newBus(t)
	b.start()
	b.appendInbox("not json\n\n{\"id\":\"noCmd\"}\n{\"cmd\":\"echo noid\"}\n[1,2]\n")
	b.request("ok", "echo fine")
	if r := b.wait("ok", ""); r.Output != "fine\n" {
		t.Fatalf("unexpected result: %+v", r)
	}
	if got := b.outbox(); len(got) != 2 {
		t.Fatalf("bad lines produced outbox entries: %+v", got)
	}
}

func TestDuplicateIDRunsOnce(t *testing.T) {
	b := newBus(t)
	counter := b.path("counter")
	b.request("a", "echo first >> "+counter)
	b.request("a", "echo second >> "+counter)
	b.start()
	b.wait("a", "")
	b.request("a", "echo third >> "+counter)
	b.settle()
	data, _ := os.ReadFile(counter)
	if string(data) != "first\n" {
		t.Fatalf("counter = %q, want only the first request to run", data)
	}
	if n := len(b.lines("a")); n != 2 {
		t.Fatalf("want 2 outbox lines for a, got %d", n)
	}
}

func TestInboxRewriteDoesNotRerun(t *testing.T) {
	b := newBus(t)
	counter := b.path("counter")
	b.start()
	b.request("a", "echo a >> "+counter)
	b.wait("a", "")

	// An editor-style rewrite: new content, old request moved and reformatted.
	la, _ := json.Marshal(Request{ID: "a", Cmd: "echo a >> " + counter})
	lb, _ := json.Marshal(Request{ID: "b", Cmd: "echo b >> " + counter})
	content := fmt.Sprintf("%s\n  %s  \n", lb, la)
	if err := os.WriteFile(b.path(inboxName), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	b.wait("b", "")
	b.settle()
	data, _ := os.ReadFile(counter)
	if string(data) != "a\nb\n" {
		t.Fatalf("counter = %q", data)
	}
}

func TestInboxDeletedAndRecreated(t *testing.T) {
	b := newBus(t)
	b.start()
	b.request("a", "echo a")
	b.wait("a", "")
	os.Remove(b.path(inboxName))
	b.settle()
	b.request("b", "echo b")
	if r := b.wait("b", ""); r.Output != "b\n" {
		t.Fatalf("unexpected result: %+v", r)
	}
}

func processGone(pid int) bool {
	if err := syscall.Kill(pid, 0); err == syscall.ESRCH {
		return true
	}
	// A zombie waiting for a slow reaper is dead for our purposes.
	stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return false
	}
	i := bytes.LastIndexByte(stat, ')')
	return i >= 0 && i+2 < len(stat) && stat[i+2] == 'Z'
}

func TestTimeoutKillsProcessGroup(t *testing.T) {
	b := newBus(t)
	b.cfg.Timeout = 300 * time.Millisecond
	b.start()
	b.request("a", "sleep 30 & echo $!; wait")
	begin := time.Now()
	r := b.wait("a", "")
	if r.Status != statusTimeout || r.Exit != -1 {
		t.Fatalf("unexpected result: %+v", r)
	}
	if took := time.Since(begin); took > 5*time.Second {
		t.Fatalf("timeout took %v", took)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(r.Output))
	if err != nil {
		t.Fatalf("no pid in output %q", r.Output)
	}
	deadline := time.Now().Add(3 * time.Second)
	for !processGone(pid) {
		if time.Now().After(deadline) {
			syscall.Kill(pid, syscall.SIGKILL)
			t.Fatalf("background child %d survived the timeout", pid)
		}
		time.Sleep(10 * time.Millisecond)
	}
	// The daemon keeps working afterwards.
	b.request("b", "echo next")
	if r := b.wait("b", ""); r.Output != "next\n" {
		t.Fatalf("unexpected result: %+v", r)
	}
}

func TestBackgroundChildHoldingOutputDoesNotHang(t *testing.T) {
	b := newBus(t)
	b.start()
	b.request("a", "sleep 30 & echo $! started")
	begin := time.Now()
	r := b.wait("a", "")
	if r.Status != statusDone || !strings.Contains(r.Output, "started") {
		t.Fatalf("unexpected result: %+v", r)
	}
	if took := time.Since(begin); took > 8*time.Second {
		t.Fatalf("took %v", took)
	}
	if pid, err := strconv.Atoi(strings.Fields(r.Output)[0]); err == nil {
		syscall.Kill(pid, syscall.SIGKILL)
	}
}

func TestRestartDoesNotRerun(t *testing.T) {
	b := newBus(t)
	counter := b.path("counter")
	stop := b.start()
	b.request("a", "echo a >> "+counter)
	b.request("b", "echo b >> "+counter+"; exit 1")
	b.wait("b", "")
	stop()

	b.start()
	b.request("c", "echo c >> "+counter)
	b.wait("c", "")
	b.settle()
	data, _ := os.ReadFile(counter)
	if string(data) != "a\nb\nc\n" {
		t.Fatalf("counter = %q", data)
	}
	if n := len(b.outbox()); n != 6 {
		t.Fatalf("want 6 outbox lines, got %d", n)
	}
}

func TestCancelMidCommandIsInterrupted(t *testing.T) {
	b := newBus(t)
	counter := b.path("counter")
	stop := b.start()
	b.request("a", "echo a >> "+counter+"; sleep 30")
	b.request("b", "echo b >> "+counter)
	b.wait("a", statusStarted)
	for b.countLines(counter) == 0 {
		time.Sleep(5 * time.Millisecond)
	}
	begin := time.Now()
	stop() // fails the test if Run does not return promptly
	if took := time.Since(begin); took > 3*time.Second {
		t.Fatalf("shutdown took %v", took)
	}
	r := b.wait("a", "")
	if r.Status != statusInterrupted || r.Exit != -1 {
		t.Fatalf("unexpected result: %+v", r)
	}
	if len(b.lines("b")) != 0 {
		t.Fatal("b must not start after shutdown began")
	}

	// Next daemon: a stays interrupted, b now runs.
	b.start()
	b.wait("b", statusDone)
	b.settle()
	data, _ := os.ReadFile(counter)
	if string(data) != "a\nb\n" {
		t.Fatalf("counter = %q", data)
	}
	if n := len(b.lines("a")); n != 2 {
		t.Fatalf("want 2 lines for a, got %d", n)
	}
}

func TestDanglingStartedIsClosedNotRerun(t *testing.T) {
	b := newBus(t)
	counter := b.path("counter")
	b.request("a", "echo a >> "+counter)
	// A daemon that died after "started" and tore its next write.
	crashed := `{"id":"a","status":"started","time":"2026-01-01T00:00:00Z"}` + "\n" + `{"id":"torn","sta`
	if err := os.WriteFile(b.path(outboxName), []byte(crashed), 0o644); err != nil {
		t.Fatal(err)
	}
	b.start()
	r := b.wait("a", "")
	if r.Status != statusInterrupted || r.Exit != -1 || r.Output == "" {
		t.Fatalf("unexpected result: %+v", r)
	}
	b.request("c", "echo c")
	if r := b.wait("c", ""); r.Output != "c\n" {
		t.Fatalf("unexpected result: %+v", r)
	}
	if b.countLines(counter) != 0 {
		t.Fatal("interrupted command was re-run")
	}
	if n := len(b.lines("a")); n != 2 {
		t.Fatalf("want 2 lines for a, got %d", n)
	}
}

func TestOutputIsCapped(t *testing.T) {
	b := newBus(t)
	b.cfg.MaxOutput = 1000
	b.start()
	b.request("a", "head -c 100000 /dev/zero | tr '\\0' x")
	r := b.wait("a", "")
	if r.Status != statusDone || !r.Truncated || len(r.Output) != 1000 {
		t.Fatalf("status=%s truncated=%v len=%d", r.Status, r.Truncated, len(r.Output))
	}
}

func TestCommandsRunInDaemonCwd(t *testing.T) {
	b := newBus(t)
	b.start()
	b.request("a", "pwd")
	cwd, _ := os.Getwd()
	if r := b.wait("a", ""); strings.TrimSpace(r.Output) != cwd {
		t.Fatalf("pwd = %q, want %q", r.Output, cwd)
	}
}

func TestLogFileIsJSONLines(t *testing.T) {
	b := newBus(t)
	logger, closer, err := newLogger(b.dir, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	b.cfg.Log = logger
	stop := b.start()
	b.appendInbox("garbage\n")
	b.request("a", "echo hi; exit 2")
	b.wait("a", "")
	stop()
	closer.Close()

	f, err := os.Open(b.path(logName))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	events := map[string]map[string]any{}
	var order []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var e map[string]any
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			t.Fatalf("log line is not JSON: %q", sc.Text())
		}
		for _, k := range []string{"time", "level", "msg"} {
			if _, ok := e[k]; !ok {
				t.Fatalf("log line lacks %q: %q", k, sc.Text())
			}
		}
		msg := e["msg"].(string)
		events[msg] = e
		order = append(order, msg)
	}
	want := "daemon_start inbox_bad_line command_start command_done daemon_stop"
	if strings.Join(order, " ") != want {
		t.Fatalf("events: %v\nwant:   %v", strings.Join(order, " "), want)
	}
	done := events["command_done"]
	if done["id"] != "a" || done["status"] != statusFailed || done["exit"] != float64(2) {
		t.Fatalf("command_done = %v", done)
	}
	if events["command_start"]["cmd"] != "echo hi; exit 2" {
		t.Fatalf("command_start = %v", events["command_start"])
	}
}
