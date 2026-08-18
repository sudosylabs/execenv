// Command execenvctl installs and manages an execenv isolation host.
//
//	execenvctl bootstrap
//	execenvctl install <id>
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
	opts := ctl.Defaults()
	root := &cobra.Command{
		Use:           "execenvctl",
		Short:         "Install and manage an execenv isolation host",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.PersistentFlags().StringVar(&opts.Prefix, "prefix", opts.Prefix, "install prefix for binaries")
	root.PersistentFlags().StringVar(&opts.Sysconf, "sysconf", opts.Sysconf, "directory for host.json and TLS files")
	root.PersistentFlags().StringVar(&opts.State, "state", opts.State, "state directory for work and images")
	root.PersistentFlags().StringVar(&opts.ReleaseURL, "release-url", "", "release asset base URL (or EXECENV_RELEASE_URL)")
	root.AddCommand(newBootstrap(&opts))
	root.AddCommand(newInstall(&opts))
	return root
}

func newBootstrap(opts *ctl.Options) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "bootstrap",
		Short: "Install the execenv daemon on this isolation host",
		Long:  "Verify the isolation device, install runtime and supervisor binaries, write host config and a systemd unit. Does not fetch catalog disks. A missing device fails closed; there is no container fallback.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return ctl.Bootstrap(*opts, cmd.OutOrStdout())
		},
	}
	cmd.Flags().StringVar(&opts.Device, "device", opts.Device, "isolation device path")
	cmd.Flags().StringVar(&opts.Listen, "listen", opts.Listen, "daemon listen address")
	cmd.Flags().IntVar(&opts.Slots, "slots", opts.Slots, "maximum concurrent grants")
	cmd.Flags().StringVar(&opts.Execenv, "execenv", opts.Execenv, "linux execenv binary to install")
	cmd.Flags().BoolVar(&opts.NoStart, "no-start", false, "write the unit but do not enable it")
	cmd.Flags().BoolVar(&opts.NoFetch, "no-fetch", false, "do not download missing runtime or supervisor binaries")
	cmd.Flags().BoolVar(&opts.Insecure, "insecure", false, "write insecure_local instead of TLS")
	return cmd
}

func newInstall(opts *ctl.Options) *cobra.Command {
	return &cobra.Command{
		Use:   "install id",
		Short: "Install one catalog image onto this host",
		Long:  "Fetch the current index entry for id from the release channel. Writes the shared kernel if missing, verifies the kernel-then-rootfs hash, and updates host config. Does not occupy grants. Ensure never fetches.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return ctl.Install(*opts, args[0], cmd.OutOrStdout())
		},
	}
}
