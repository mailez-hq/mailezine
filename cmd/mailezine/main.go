// Command mailezine is the mailez first-party mail engine (engine C).
//
// The composition root lives in internal/app; this package only parses
// flags, installs signal handling and translates errors into exit codes.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"mailezine/internal/app"
	"mailezine/internal/config"
	"mailezine/internal/version"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if len(os.Args) > 1 && os.Args[1] == "migrate" {
		os.Exit(runMigrate(os.Args[2:]))
	}
	if len(os.Args) > 1 && os.Args[1] == "reindex" {
		os.Exit(runReindex(os.Args[2:]))
	}
	if len(os.Args) > 1 && os.Args[1] == "backup" {
		os.Exit(runBackup(os.Args[2:]))
	}
	if len(os.Args) > 1 && os.Args[1] == "restore" {
		os.Exit(runRestore(os.Args[2:]))
	}
	os.Exit(runCtx(ctx, os.Args[1:]))
}

func runCtx(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("mailezine", flag.ContinueOnError)
	showVersion := fs.Bool("version", false, "print version and exit")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *showVersion {
		fmt.Println(version.String())
		return 0
	}

	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "mailezine: %v\n", err)
		return 2
	}
	a, err := app.New(ctx, cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "mailezine: %v\n", err)
		return 2
	}
	defer a.Close()
	if err := a.Run(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "mailezine: %v\n", err)
		return 1
	}
	return 0
}
