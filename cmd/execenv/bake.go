package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"runtime"

	"github.com/sudosylabs/execenv"
	"github.com/sudosylabs/execenv/bake"
)

func runBake(args []string) int {
	fs := flag.NewFlagSet("bake", flag.ContinueOnError)
	out := fs.String("out", "", "directory for vmlinux, rootfs.ext4, and catalog.json")
	kernel := fs.String("kernel", "", "already-built kernel to copy as vmlinux")
	source := fs.String("source", "", "container image to export (default: universal-class pin)")
	dockerfile := fs.String("dockerfile", "", "operator Dockerfile for an extra catalog id")
	id := fs.String("id", bake.DefaultID, "catalog id")
	agentPath := fs.String("agent", "", "execenv binary to install; default is this process")
	sizeText := fs.String("size", "", "rootfs size, for example 20G; default is staging plus headroom")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *out == "" || *kernel == "" {
		fmt.Fprintln(os.Stderr, "usage: execenv bake -out <dir> -kernel <vmlinux> [-source <image> | -dockerfile <file>] [-id default] [-size 20G]")
		fmt.Fprintln(os.Stderr, "  -size defaults to twice the exported tree plus 64MiB. Universal-class disks need tens of gigabytes.")
		fmt.Fprintln(os.Stderr, "  -agent must be a linux execenv if this process is not linux.")
		return 2
	}
	size, err := bake.ParseSize(*sizeText)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	agentBin := *agentPath
	if agentBin == "" {
		if runtime.GOOS != "linux" {
			fmt.Fprintln(os.Stderr, "bake on this OS needs -agent pointing at a linux execenv binary")
			return 2
		}
		agentBin, err = os.Executable()
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
	}
	if !bake.LinuxAgent(agentBin) {
		fmt.Fprintln(os.Stderr, "agent is not a linux ELF binary")
		return 2
	}
	req := bake.Request{
		ID:         execenv.Image(*id),
		Source:     *source,
		Dockerfile: *dockerfile,
		Kernel:     *kernel,
		Agent:      agentBin,
		OutDir:     *out,
		Size:       size,
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	res, err := bake.Run(ctx, req)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	// Hash is the catalog digest, not a grant token or file body.
	fmt.Printf("id=%s kernel=%s rootfs=%s hash=%s source=%s\n",
		res.ID, res.Kernel, res.Rootfs, res.Hash, res.Source)
	return 0
}
