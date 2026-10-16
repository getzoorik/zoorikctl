package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/getzoorik/zoorikctl/internal/cli"
	"github.com/getzoorik/zoorikctl/internal/ui"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	streams := cli.Streams{
		In:          os.Stdin,
		Out:         os.Stdout,
		Err:         os.Stderr,
		Interactive: ui.IsTerminal(os.Stdin) && ui.IsTerminal(os.Stderr),
	}
	code := cli.Main(ctx, os.Args[1:], streams)
	stop()
	os.Exit(code)
}
