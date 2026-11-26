//go:build !linux

package workload

import (
	"context"
	"os"
	"os/exec"

	"github.com/sudosylabs/execenv"
)

type Group struct{}

func Open() (*Group, error)                                { return nil, execenv.ErrUnavailable }
func (*Group) Stopped(context.Context, func() error) error { return execenv.ErrUnavailable }
func (*Group) Configure(*exec.Cmd) error                   { return execenv.ErrUnavailable }
func (*Group) OwnHome(string) error                        { return execenv.ErrUnavailable }
func (*Group) OwnPath(*os.Root, string) error              { return execenv.ErrUnavailable }
func (*Group) Path() string                                { return "" }
func (*Group) Kill(context.Context) error                  { return execenv.ErrUnavailable }
func (*Group) Close() error                                { return nil }
func Exec([]string) error                                  { return execenv.ErrUnavailable }

func (*Group) Check() error { return execenv.ErrUnavailable }
