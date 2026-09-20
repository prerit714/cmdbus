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

## Quick start

```sh
go build -o cmdbus .          # Go 1.21+; Linux or macOS

cd /path/to/your/repo
/path/to/cmdbus               # runs in the foreground; Ctrl-C stops it
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

Commands run with `sh -c` in the directory cmdbus was started from. The folder and files
are created on start if missing.

## Protocol

*This section is self-contained — paste it into the AI's instructions.*

To run a command on the host, append **one line** to `.cmdbus/inbox.jsonl`:

```json
{"id":"build-1","cmd":"go test ./..."}
```

- `id` — any string you have not used before. A reused id is ignored.
- `cmd` — a shell command, run with `sh -c` from the repository root on the host.
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

- Unix only (uses process groups and `sh`).
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
| `main.go`        | flags, logger, signal handling                       |
| `daemon.go`      | inbox/outbox handling, poll loop, recovery, exec     |
| `daemon_test.go` | end-to-end tests                                     |
