// Command execenvctl installs and manages an execenv isolation host.
//
//	execenvctl bootstrap
//
// This is not the daemon and not the guest agent. It does not occupy
// grants. Flags and exit codes only; there is no TUI.
package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
	"github.com/sudosylabs/execenv/internal/ctl"
)

func main() {
	cmd := newRoot()
	if err := cmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func newRoot() *cobra.Command {
	root := &cobra.Command{
		Use:           "execenvctl",
		Short:         "Install and manage an execenv isolation host",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.AddCommand(newBootstrap())
	return root
}

func newBootstrap() *cobra.Command {
	opts := ctl.Defaults()
	cmd := &cobra.Command{
		Use:   "bootstrap",
		Short: "Install the execenv daemon on this isolation host",
		Long:  "Verify the isolation device, install runtime and supervisor binaries, write host config and a systemd unit. Does not fetch catalog disks. A missing device fails closed; there is no container fallback.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return ctl.Bootstrap(opts, cmd.OutOrStdout())
		},
	}
	cmd.Flags().StringVar(&opts.Device, "device", opts.Device, "isolation device path")
	cmd.Flags().StringVar(&opts.Prefix, "prefix", opts.Prefix, "install prefix for binaries")
	cmd.Flags().StringVar(&opts.Sysconf, "sysconf", opts.Sysconf, "directory for host.json and TLS files")
	cmd.Flags().StringVar(&opts.State, "state", opts.State, "state directory for work and images")
	cmd.Flags().StringVar(&opts.Listen, "listen", opts.Listen, "daemon listen address")
	cmd.Flags().IntVar(&opts.Slots, "slots", opts.Slots, "maximum concurrent grants")
	cmd.Flags().StringVar(&opts.Execenv, "execenv", opts.Execenv, "linux execenv binary to install")
	cmd.Flags().BoolVar(&opts.NoStart, "no-start", false, "write the unit but do not enable it")
	cmd.Flags().BoolVar(&opts.NoFetch, "no-fetch", false, "do not download missing runtime or supervisor binaries")
	cmd.Flags().BoolVar(&opts.Insecure, "insecure", false, "write insecure_local instead of TLS")
	return cmd
}
