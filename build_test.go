package execenv

import "testing"

func TestParseModuleVersion(t *testing.T) {
	t.Parallel()
	cases := []struct {
		raw, version, tag string
	}{
		{"", "dev", ""},
		{"(devel)", "dev", ""},
		{"v1.2.3", "1.2.3", "v1.2.3"},
		{"v1.2.3-rc.1", "1.2.3-rc.1", "v1.2.3-rc.1"},
		{"v0.0.0-20240101120000-abcdefabcdef", "dev", ""},
		{"v1.2.3-0.20240101120000-abcdefabcdef", "dev", ""},
		{"v0.0.0-20260819151636-e793f7f44be0+dirty", "dev", ""},
		{"v1.2.3+incompatible", "1.2.3", "v1.2.3"},
		{"1.2.3", "dev", ""},
	}
	for _, tc := range cases {
		ver, tag := parseModuleVersion(tc.raw)
		if ver != tc.version || tag != tc.tag {
			t.Fatalf("parseModuleVersion(%q) = %q, %q; want %q, %q",
				tc.raw, ver, tag, tc.version, tc.tag)
		}
	}
}

func TestUnstampedVars(t *testing.T) {
	t.Parallel()
	if Release != stampDev {
		t.Fatalf("Release = %q, want %q", Release, stampDev)
	}
	if Build != stampDev {
		t.Fatalf("Build = %q, want %q", Build, stampDev)
	}
	if Tag != "" {
		t.Fatalf("Tag = %q, want empty", Tag)
	}
}
