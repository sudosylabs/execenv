package ctl

import "testing"

func TestManagerCommandsShareTheHostConfigurationLock(t *testing.T) {
	opts := testOpts(t, t.TempDir())
	first, err := opts.lockConfig()
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	if second, err := opts.lockConfig(); err == nil {
		_ = second.Close()
		t.Fatal("a second manager acquired the host configuration lock")
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	next, err := opts.lockConfig()
	if err != nil {
		t.Fatal(err)
	}
	_ = next.Close()
}
