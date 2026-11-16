// The upnp command looks at the UPnP devices of the local network by
// hand: which there are, what each says of itself, what its actions
// answer and what its events tell. Run it with no argument for how.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/forkcloser/go-upnp/internal/command"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)

	code := command.Run(ctx, command.Environment{Args: os.Args[1:], Out: os.Stdout, Err: os.Stderr})

	stop()
	os.Exit(code)
}
