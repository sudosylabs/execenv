//go:build !linux

package isolated

import "github.com/sudosylabs/execenv"

func probeSandbox(Config) error { return execenv.ErrUnavailable }

func mountImageRoot(string, string) error { return execenv.ErrUnavailable }

func mountWorkspaceDisk(string, string) error { return execenv.ErrUnavailable }

func unmountPath(string) error { return nil }

func unmountUnder(string) error { return nil }
