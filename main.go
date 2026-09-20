// cmdbus runs shell commands requested through a JSONL file and appends their
// results to another. See README.md, or run `cmdbus docs`.
package main

import (
	"context"
	_ "embed"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"
)

// docs is the complete documentation, printed by `cmdbus docs`.
//
//go:embed README.md
var docs string

func main() {
	dir := flag.String("dir", ".cmdbus", "directory holding inbox.jsonl, outbox.jsonl and cmdbus.log")
	timeout := flag.Duration("timeout", 5*time.Minute, "time limit per command")
	flag.Usage = func() {
		fmt.Fprintln(flag.CommandLine.Output(), "usage:\n  cmdbus [-dir .cmdbus] [-timeout 5m]   run the daemon in the foreground (Ctrl-C stops it)\n  cmdbus docs                           print the complete documentation (markdown)\n\nflags:")
		flag.PrintDefaults()
	}
	flag.Parse()

	if flag.NArg() == 1 && flag.Arg(0) == "docs" {
		fmt.Print(docs)
		return
	}
	if flag.NArg() > 0 || *timeout <= 0 {
		flag.Usage()
		os.Exit(2)
	}

	logger, logFile, err := newLogger(*dir, os.Stderr)
	if err != nil {
		fmt.Fprintln(os.Stderr, "cmdbus:", err)
		os.Exit(1)
	}
	defer logFile.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	err = Run(ctx, Config{
		Dir:       *dir,
		Timeout:   *timeout,
		Poll:      250 * time.Millisecond,
		MaxOutput: 1 << 20,
		Log:       logger,
	})
	if err != nil {
		logger.Error("daemon_stop", "reason", err.Error())
		logFile.Close()
		os.Exit(1)
	}
}
