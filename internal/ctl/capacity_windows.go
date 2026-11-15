package ctl

// diskFree is not measured on Windows, where no host is bootstrapped.
func diskFree(string) (int64, bool) { return 0, false }
