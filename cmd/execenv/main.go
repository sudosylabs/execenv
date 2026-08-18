// Command execenv is the host daemon, the guest agent, and the operator
// bake step. One binary is intentional: bake copies this program into the
// guest disk; the guest starts it as `agent`; the host machine starts
// it as the daemon. Bake never runs inside Ensure.
//
//	execenv -config /etc/execenv/host.json
//	execenv agent -home /workspace
//	execenv bake -out DIR -kernel vmlinux
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
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "agent":
			os.Exit(runAgent(os.Args[2:]))
		case "bake":
			os.Exit(runBake(os.Args[2:]))
		}
	}
	configPath := flag.String("config", "", "path to the host JSON config")
	flag.Parse()
	if *configPath == "" {
		fmt.Fprintln(os.Stderr, "usage: execenv -config <path>")
		fmt.Fprintln(os.Stderr, "       execenv agent -home <dir> [-listen <unix-socket>]")
		fmt.Fprintln(os.Stderr, "       execenv bake -out <dir> -kernel <vmlinux> [-source <image> | -dockerfile <file>]")
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
