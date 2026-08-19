package execenv

import (
	"runtime/debug"
	"strings"
)

const (
	modulePath = "github.com/sudosylabs/execenv"
	stampDev   = "dev"
)

// Release, Build, and Tag identify this module and the binaries built
// from it. Release is the module pin (1.2.3); Version is a tree token
// and cannot be this var. They are vars so `go build -ldflags -X` can
// set them. Empty means unstamped: init fills Release from module build
// info, then reports "dev" and an empty Tag when that is not a release.
var (
	Release string
	Build   string
	Tag     string
)

func init() {
	applyBuildInfo()
	if Release == "" {
		Release = stampDev
	}
	if Build == "" {
		Build = stampDev
	}
}

// Stamp is the operator-facing identity. It never includes tokens or
// catalog hashes.
func Stamp() string {
	return "version=" + Release + "\nbuild=" + Build + "\ntag=" + Tag + "\n"
}

func applyBuildInfo() {
	if Release != "" {
		return
	}
	info, ok := debug.ReadBuildInfo()
	if !ok {
		Release = stampDev
		return
	}
	ver, tag := parseModuleVersion(moduleVersion(info))
	Release = ver
	if Tag == "" {
		Tag = tag
	}
}

func moduleVersion(info *debug.BuildInfo) string {
	if info.Main.Path == modulePath {
		return info.Main.Version
	}
	for _, d := range info.Deps {
		if d.Path == modulePath {
			return d.Version
		}
	}
	return ""
}

func parseModuleVersion(raw string) (version, tag string) {
	if i := strings.Index(raw, "+"); i >= 0 {
		raw = raw[:i]
	}
	if raw == "" || raw == "(devel)" {
		return stampDev, ""
	}
	if isPseudoVersion(raw) {
		return stampDev, ""
	}
	if !strings.HasPrefix(raw, "v") {
		return stampDev, ""
	}
	return strings.TrimPrefix(raw, "v"), raw
}

func isPseudoVersion(raw string) bool {
	i := strings.LastIndex(raw, "-")
	if i < 0 || strings.Count(raw, "-") < 2 {
		return false
	}
	suf := raw[i+1:]
	if len(suf) != 12 {
		return false
	}
	for i := 0; i < len(suf); i++ {
		c := suf[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
