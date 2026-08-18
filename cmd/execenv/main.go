// Command execenv starts a host daemon from a JSON config file, or the
// guest-side agent that owns one grant's home and login shell.
//
//	execenv -config /etc/execenv/host.json
//	execenv agent -home /workspace -listen /run/execenv/agent.sock
//
// The daemon serves the execenv remote contract. It does not occupy grants
// until it has bound its listen address. One binary is intentional: the
// guest image starts this same program as `agent` so the host can dial a
// real PTY.
package main

import (
	"context"
	"flag"
	"fmt"
	"net"
	"os"
	"os/signal"

	"github.com/sudosylabs/execenv/agent"
	"github.com/sudosylabs/execenv/daemon"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "agent" {
		os.Exit(runAgent(os.Args[2:]))
	}
	configPath := flag.String("config", "", "path to the host JSON config")
	flag.Parse()
	if *configPath == "" {
		fmt.Fprintln(os.Stderr, "usage: execenv -config <path>")
		fmt.Fprintln(os.Stderr, "       execenv agent -home <dir> -listen <unix-socket>")
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := daemon.Main(ctx, *configPath); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func runAgent(args []string) int {
	fs := flag.NewFlagSet("agent", flag.ContinueOnError)
	home := fs.String("home", "", "projected home directory")
	listen := fs.String("listen", "", "unix socket the host dials; empty uses the guest link")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *home == "" {
		fmt.Fprintln(os.Stderr, "usage: execenv agent -home <dir> [-listen <unix-socket>]")
		return 2
	}
	var ln net.Listener
	var err error
	if *listen != "" {
		_ = os.Remove(*listen)
		ln, err = net.Listen("unix", *listen)
	} else {
		ln, err = agent.ListenGuest()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := agent.ListenAndServe(ctx, ln, agent.Config{Home: *home}); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}
