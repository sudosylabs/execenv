// Command execenv starts a host daemon from a JSON config file.
//
//	execenv -config /etc/execenv/host.json
//
// The process serves the execenv remote contract. It does not occupy grants
// until it has bound its listen address.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"

	"github.com/sudosylabs/execenv/daemon"
)

func main() {
	configPath := flag.String("config", "", "path to the host JSON config")
	flag.Parse()
	if *configPath == "" {
		fmt.Fprintln(os.Stderr, "usage: execenv -config <path>")
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := daemon.Main(ctx, *configPath); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
