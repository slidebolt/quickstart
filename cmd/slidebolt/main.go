package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/slidebolt/quickstart/app"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	executable, _ := os.Executable()
	application := app.App{In: os.Stdin, Out: os.Stdout, Err: os.Stderr, Executable: executable}
	os.Exit(application.Run(ctx, os.Args[1:]))
}
