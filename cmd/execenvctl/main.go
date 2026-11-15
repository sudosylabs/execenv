// Command execenvctl installs and manages an execenv isolation host.
//
//	execenvctl bootstrap
//	execenvctl install <id> [--force]
//	execenvctl list
//	execenvctl remove <id> [--force]
//	execenvctl status
//	execenvctl upgrade [--force]
//	execenvctl drain [--timeout 15m]
//	execenvctl undrain
//
// install, remove and upgrade restart a running daemon, which destroys
// every grant it holds, so they refuse while grants are live unless
// --force is given. drain stops the daemon taking new grants and waits for
// the live ones to end; a drained daemon restarts without --force.
//
// This is not the daemon and not the guest agent. It does not occupy
// grants. Flags and exit codes only; there is no TUI.
package main

import (
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"
	"github.com/sudosylabs/execenv"
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
		Version:       execenv.Release,
	}
	root.SetVersionTemplate(execenv.Stamp())
	root.PersistentFlags().StringVar(&opts.Prefix, "prefix", opts.Prefix, "install prefix for binaries")
	root.PersistentFlags().StringVar(&opts.Sysconf, "sysconf", opts.Sysconf, "directory for host.json and TLS files")
	root.PersistentFlags().StringVar(&opts.State, "state", opts.State, "state directory for work and images")
	root.PersistentFlags().StringVar(&opts.ReleaseURL, "release-url", "", "release asset base URL (or EXECENV_RELEASE_URL)")
	root.AddCommand(newBootstrap(&opts))
	root.AddCommand(newInstall(&opts))
	root.AddCommand(newList(&opts))
	root.AddCommand(newRemove(&opts))
	root.AddCommand(newStatus(&opts))
	root.AddCommand(newUpgrade(&opts))
	root.AddCommand(newDrain(&opts))
	root.AddCommand(newUndrain(&opts))
	return root
}

func newBootstrap(opts *ctl.Options) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "bootstrap",
		Short: "Install the execenv daemon on this isolation host",
		Long:  "Verify the isolation device, install runtime and supervisor binaries, write host config and a systemd unit. Does not fetch catalog disks. A missing device fails closed (no container fallback).",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return ctl.Bootstrap(*opts, cmd.OutOrStdout())
		},
	}
	cmd.Flags().StringVar(&opts.Device, "device", opts.Device, "isolation device path")
	cmd.Flags().StringVar(&opts.Mkfs, "mkfs", "", "mkfs.ext4 used for per-grant workspace disks (default: PATH)")
	cmd.Flags().StringVar(&opts.Listen, "listen", opts.Listen, "daemon listen address")
	cmd.Flags().Int64Var(&opts.Capacity.CPUMillis, "cpu-millis", 0, "CPU the host gives grants in all, 1000 per CPU (default: every CPU)")
	cmd.Flags().Int64Var(&opts.Capacity.MemoryBytes, "memory-bytes", 0, "memory the host gives grants in all (default: the machine's, less a reserve)")
	cmd.Flags().Int64Var(&opts.Capacity.DiskBytes, "disk-bytes", 0, "workspace disk the host gives grants in all (default: 90% of free space)")
	cmd.Flags().StringVar(&opts.Execenv, "execenv", opts.Execenv, "linux execenv binary to install")
	cmd.Flags().BoolVar(&opts.NoStart, "no-start", false, "write the unit but do not enable it")
	cmd.Flags().BoolVar(&opts.NoFetch, "no-fetch", false, "do not download missing runtime or supervisor binaries")
	cmd.Flags().BoolVar(&opts.Insecure, "insecure", false, "write insecure_local instead of TLS")
	return cmd
}

// restartNote is the shared help for commands that restart the daemon.
const restartNote = " Restarts a running daemon, which destroys every grant it holds, so it refuses before changing anything while the daemon holds grants. Run execenvctl drain first, or pass --force to restart anyway."

func forceFlag(cmd *cobra.Command, opts *ctl.Options) {
	cmd.Flags().BoolVar(&opts.Force, "force", false, "restart the daemon even while it holds grants, destroying them")
}

func newInstall(opts *ctl.Options) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "install id",
		Short: "Install one catalog image onto this host",
		Long:  "Fetch the current index entry for id from the release channel. Writes the shared kernel if missing, verifies the kernel-then-rootfs hash, and updates host config. Does not occupy grants. Ensure never fetches." + restartNote,
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return ctl.Install(*opts, args[0], cmd.OutOrStdout())
		},
	}
	forceFlag(cmd, opts)
	return cmd
}

func newList(opts *ctl.Options) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "Show installed and published catalog ids",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return ctl.List(*opts, cmd.OutOrStdout())
		},
	}
}

func newRemove(opts *ctl.Options) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "remove id",
		Short: "Remove one catalog image from this host",
		Long:  "Drop id from host config and delete its disk. The shared kernel stays while another image uses it." + restartNote,
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return ctl.Remove(*opts, args[0], cmd.OutOrStdout())
		},
	}
	forceFlag(cmd, opts)
	return cmd
}

func newStatus(opts *ctl.Options) *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show device, unit, drain state, live grants and installed ids",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return ctl.Status(*opts, cmd.OutOrStdout())
		},
	}
}

func newUpgrade(opts *ctl.Options) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "upgrade",
		Short: "Replace execenvctl and execenv from the release channel",
		Long:  "Fetch linux/amd64 binaries and reinstall every catalog id already on the host from this binary's tag, then restart the daemon once. The host token and TLS files are left alone." + restartNote,
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return ctl.Upgrade(*opts, cmd.OutOrStdout())
		},
	}
	forceFlag(cmd, opts)
	return cmd
}

func newDrain(opts *ctl.Options) *cobra.Command {
	var timeout time.Duration
	cmd := &cobra.Command{
		Use:   "drain",
		Short: "Stop taking new grants and wait for live ones to end",
		Long: "Tell the local daemon to refuse new grants (callers see ErrDraining and an unusable Ready) while it keeps serving the grants it holds, " +
			"then wait until it holds none, printing the remaining count as it changes. " +
			"Exits non-zero if --timeout passes first; the daemon stays draining either way. " +
			"--timeout 0 starts draining and returns at once. A drained daemon restarts without --force; " +
			"the restart brings it back serving, and execenvctl undrain does so without one.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return ctl.Drain(*opts, timeout, cmd.OutOrStdout())
		},
	}
	cmd.Flags().DurationVar(&timeout, "timeout", ctl.DefaultDrainTimeout, "how long to wait for live grants to end")
	return cmd
}

func newUndrain(opts *ctl.Options) *cobra.Command {
	return &cobra.Command{
		Use:   "undrain",
		Short: "Take new grants again after a drain",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return ctl.Undrain(*opts, cmd.OutOrStdout())
		},
	}
}
