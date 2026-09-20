# cmdbus

Run commands on your host machine from a sandbox that can only touch files.

AI desktop apps (Claude, ChatGPT, …) work on your repository inside a Linux sandbox. The
sandbox can read and write the repo's files, but it cannot reach your real toolchain, your
Docker daemon, your devices or your network. `cmdbus` bridges that gap with nothing but two
files in the shared folder:

```
        sandbox                     shared repo folder                     host
  ┌────────────────┐   append   ┌──────────────────────┐    poll    ┌────────────────┐
  │  AI assistant  │ ─────────▶ │ .cmdbus/inbox.jsonl  │ ─────────▶ │                │
  │                │            │                      │            │     cmdbus     │
  │                │ ◀───────── │ .cmdbus/outbox.jsonl │ ◀───────── │    (sh -c)     │
  └────────────────┘    read    └──────────────────────┘   append   └────────────────┘
```

Every request gets exactly one result, matched by `id`. One Go binary, standard library
only, no database, no network, no background service.

> **Security.** cmdbus executes whatever is written to the inbox, with your user's
> permissions. It is a deliberate hole in the sandbox. Only run it against a folder shared
> with a sandbox you would trust with your shell, and stop it (Ctrl-C) when you are done.

## Install

The install scripts clone this repository, build the binary and add it to your user PATH.
They need `git` and Go 1.21+. The repository is private, so git must be able to reach it
(an SSH key or a credential helper).

Linux / macOS:

```sh
sh install.sh
```

Windows (PowerShell):

```powershell
powershell -ExecutionPolicy Bypass -File install.ps1
```

|               | source                         | binary                         | PATH is added to                          |
|---------------|--------------------------------|--------------------------------|-------------------------------------------|
| `install.sh`  | `~/.local/share/cmdbus`        | `~/.local/bin/cmdbus`          | `~/.bashrc`, `~/.zshrc`, fish config or `~/.profile` |
| `install.ps1` | `%LOCALAPPDATA%\cmdbus\src`    | `%LOCALAPPDATA%\cmdbus\bin`    | the user `Path` environment variable      |

Override with `CMDBUS_REPO`, `CMDBUS_SRC` and `CMDBUS_BIN`. Running a script again updates
the clone and rebuilds. To build by hand instead: `go build -o cmdbus .`

## Quick start

```sh
cd /path/to/your/repo
cmdbus                        # runs in the foreground; Ctrl-C stops it
```

From anywhere that can see the folder (the sandbox, or a second terminal):

```sh
echo '{"id":"t1","cmd":"uname -a; exit 3"}' >> .cmdbus/inbox.jsonl
cat .cmdbus/outbox.jsonl
```

```json
{"id":"t1","status":"started","time":"2026-09-20T16:59:07Z"}
{"id":"t1","status":"failed","exit":3,"duration_ms":2,"output":"Linux host 7.0.0 ...\n","truncated":false,"time":"2026-09-20T16:59:07Z"}
```

Add `.cmdbus/` to the repo's `.gitignore`.

### Options

| flag       | default   | meaning                                                   |
|------------|-----------|-----------------------------------------------------------|
| `-dir`     | `.cmdbus` | folder holding `inbox.jsonl`, `outbox.jsonl`, `cmdbus.log` |
| `-timeout` | `5m`      | time limit per command                                    |

Commands run with `sh -c` (on Windows: `powershell -NoProfile -NonInteractive`) in the
directory cmdbus was started from. The folder and files are created on start if missing.

### Exporting this documentation

The binary carries this whole document. Print it, or write it where the AI can read it:

```sh
cmdbus docs                   # markdown on stdout
cmdbus docs > CMDBUS.md
```

## Protocol

*This section is self-contained — paste it into the AI's instructions.*

To run a command on the host, append **one line** to `.cmdbus/inbox.jsonl`:

```json
{"id":"build-1","cmd":"go test ./..."}
```

- `id` — any string you have not used before. A reused id is ignored.
- `cmd` — a shell command for the host's shell (`sh` on Linux/macOS, PowerShell on
  Windows), run from the directory cmdbus was started in — normally the repository root.
- The line must be valid JSON and must end with a newline.
- Never write to `outbox.jsonl`.

Then re-read `.cmdbus/outbox.jsonl` until it contains a line with your `id` whose `status`
is not `started`. That line is the result:

```json
{"id":"build-1","status":"started","time":"2026-09-20T15:30:12Z"}
{"id":"build-1","status":"done","exit":0,"duration_ms":1240,"output":"ok\n","truncated":false,"time":"2026-09-20T15:30:13Z"}
```

| status        | meaning                                                                 |
|---------------|-------------------------------------------------------------------------|
| `started`     | running now; the final line will follow                                 |
| `done`        | exit code 0                                                             |
| `failed`      | non-zero exit code, given in `exit`                                     |
| `timeout`     | killed after the time limit; `exit` is -1                               |
| `interrupted` | the daemon was stopped while it ran; it will not be re-run; `exit` is -1 |

- `output` is stdout and stderr combined, at most 1 MiB; `truncated` is `true` if it was cut.
- Requests run one at a time, in the order they appear in the inbox.
- To retry a command, send it again under a new `id`.
- If no `started` line appears within a few seconds, the daemon is not running — ask the
  user to start `cmdbus` on the host.

## Guarantees

- **One-to-one.** Every valid inbox line gets exactly one final outbox line.
- **At most once.** `started` is recorded before a command runs, and an id present in the
  outbox is never run again — across restarts, crashes, Ctrl-C, duplicate ids, and the inbox
  being rewritten, reordered or deleted. A command that was cut off is reported as
  `interrupted`, never silently repeated.
- **The outbox is the only state.** On start the daemon rebuilds everything from the two
  files. There is nothing else to corrupt or get out of sync.
- **No locks.** Each file has a single writer (inbox: sandbox, outbox: daemon), and every
  outbox entry is one complete line written with a single `write`.
- **Tolerant reader.** A line without a trailing newline is treated as still being written
  and left alone. An invalid line is logged once and skipped; later lines still run.
- **Clean kills.** Timeout and Ctrl-C kill the command's whole process group, so nothing
  it spawned is left behind.

## Why these choices

- **JSONL, not Markdown or one JSON document.** A JSON-lines file can be appended to, each
  line is either complete or not, and `encoding/json` does all parsing and escaping. Markdown
  would need a custom parser and fence escaping; a single JSON document must be rewritten
  whole on every change and is invalid while half-written.
- **Polling, not inotify.** File events generally do not cross sandbox mounts (virtiofs,
  9p, bind mounts). The inbox is small, so it is simply re-read every 250 ms.
- **Whole-file re-read, not tailing.** AI edit tools often rewrite a file instead of
  appending, which makes byte offsets meaningless. Ids make re-reading safe.
- **No shared database.** File locking is unreliable across those same mounts, and two
  single-writer files do not need it.

## Logs

Structured JSON, one event per line, on stderr and appended to `<dir>/cmdbus.log`:

```sh
tail -f .cmdbus/cmdbus.log | jq
```

| `msg`                   | fields                                                                  |
|-------------------------|-------------------------------------------------------------------------|
| `daemon_start`          | `dir`, `cwd`, `timeout`, `pid`                                          |
| `command_start`         | `id`, `cmd`                                                             |
| `command_done`          | `id`, `status`, `exit`, `duration_ms`, `output_bytes`, `truncated`      |
| `recovered_interrupted` | `id` — a command the previous daemon never finished                     |
| `inbox_bad_line`        | `line`, `reason`                                                        |
| `error`                 | `op`, `err`                                                             |
| `daemon_stop`           | `reason`                                                                |

## Limits

- Windows support compiles but has not been run on a Windows machine yet; the test suite
  is Unix only.
- Commands get no stdin and no TTY; interactive programs will not work.
- One command at a time. A long-running command blocks the ones behind it until it
  finishes or hits `-timeout`.
- The inbox and outbox grow forever. Stop the daemon and delete `.cmdbus/` to start fresh.
- A command that leaves a background process holding its stdout is given 2 s after the
  shell exits, then reported with the output collected so far.

## Development

```sh
go vet ./... && go test -race ./...
```

The tests start a real daemon against a temp directory and real `sh`, and cover every
guarantee above: ordering, exit codes, awkward output (quotes, backticks, JSON, invalid
UTF-8), unfinished and invalid lines, duplicate ids, inbox rewrite and deletion, timeout
and process-group kill, restart, crash recovery, Ctrl-C mid-command, the output cap, and
the log format. Run them after every change; all of them must pass.

| file             | contents                                             |
|------------------|------------------------------------------------------|
| `main.go`                          | flags, `docs` command, logger, signal handling   |
| `daemon.go`                        | inbox/outbox handling, poll loop, recovery, exec |
| `proc_unix.go` / `proc_windows.go` | how a command is started and killed per OS       |
| `daemon_test.go`, `main_test.go`   | end-to-end tests                                 |
| `install.sh` / `install.ps1`       | clone, build, add to PATH                        |
