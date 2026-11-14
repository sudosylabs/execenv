//go:build !linux

package isolated

func unmountUnder(string) error { return nil }
