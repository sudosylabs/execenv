// Command execenv is the host daemon and the guest agent. One binary is
// intentional: CI copies this program into the guest disk; the guest
// starts it as `agent`; the host machine starts it as the daemon.
//
//	execenv -config /etc/execenv/host.json
//	execenv agent -home /workspace -scratch /run/execenv/home
//
// Catalog disks are produced by scripts/bake in CI. A Linux host is
// installed by execenvctl. Neither is this command.
package main

import (
	"context"
	"flag"
	"fmt"
	"net"
	"os"
	"os/signal"

	"github.com/sudosylabs/execenv"
	"github.com/sudosylabs/execenv/agent"
	"github.com/sudosylabs/execenv/daemon"
	"github.com/sudosylabs/execenv/internal/workload"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "guest-init" {
		if err := workload.RunInit(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	if len(os.Args) == 2 && os.Args[1] == "workload-shell" {
		if err := workload.ExecShell(); err != nil {
			os.Exit(1)
		}
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "agent" {
		os.Exit(runAgent(os.Args[2:]))
	}
	showVersion := flag.Bool("version", false, "print version and exit")
	configPath := flag.String("config", "", "path to the host JSON config")
	flag.Parse()
	if *showVersion {
		fmt.Print(execenv.Stamp())
		return
	}
	if *configPath == "" {
		fmt.Fprintln(os.Stderr, "usage: execenv -config <path>")
		fmt.Fprintln(os.Stderr, "       execenv agent -home <dir> [-scratch <dir>] [-listen <unix-socket>]")
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
	home := fs.String("home", "", "projected tree; the shell's working directory")
	scratch := fs.String("scratch", "", "the shell's $HOME, outside the projected tree; empty uses -home")
	contained := fs.Bool("contain-workload", false, "require a kernel-acknowledged unprivileged workload barrier")
	listen := fs.String("listen", "", "unix socket the host dials; empty uses the guest link")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *home == "" {
		fmt.Fprintln(os.Stderr, "usage: execenv agent -home <dir> [-scratch <dir>] [-listen <unix-socket>]")
		return 2
	}
	cfg := agent.Config{Home: *home, Scratch: *scratch, ContainWorkload: *contained}
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
	if err := agent.ListenAndServe(ctx, ln, cfg); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}
