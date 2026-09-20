// cmdbus runs shell commands requested through a JSONL file and appends their
// results to another. See README.md.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	dir := flag.String("dir", ".cmdbus", "directory holding inbox.jsonl, outbox.jsonl and cmdbus.log")
	timeout := flag.Duration("timeout", 5*time.Minute, "time limit per command")
	flag.Parse()
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
