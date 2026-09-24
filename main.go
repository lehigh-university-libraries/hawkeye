package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
)

var version = "dev"

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := newCommand().ExecuteContext(ctx); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
