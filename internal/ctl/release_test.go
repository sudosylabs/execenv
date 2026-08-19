package ctl

import (
	"strings"
	"testing"

	"github.com/sudosylabs/execenv"
)

func TestResolveReleaseURLPrefersFlag(t *testing.T) {
	t.Parallel()
	got, err := resolveReleaseURL("https://mirror.example/assets/", "", "v1.2.3")
	if err != nil {
		t.Fatal(err)
	}
	if got != "https://mirror.example/assets" {
		t.Fatalf("got %q", got)
	}
}

func TestResolveReleaseURLUsesEnv(t *testing.T) {
	t.Parallel()
	got, err := resolveReleaseURL("", "https://mirror.example/from-env", "v1.2.3")
	if err != nil {
		t.Fatal(err)
	}
	if got != "https://mirror.example/from-env" {
		t.Fatalf("got %q", got)
	}
}

func TestResolveReleaseURLTaggedGitHub(t *testing.T) {
	t.Parallel()
	got, err := resolveReleaseURL("", "", "v1.2.3")
	if err != nil {
		t.Fatal(err)
	}
	if got != githubReleaseDownload+"/v1.2.3" {
		t.Fatalf("got %q", got)
	}
	if strings.Contains(got, "latest") {
		t.Fatal("tagged URL used latest")
	}
}

func TestResolveReleaseURLUnstampedRefusesGitHub(t *testing.T) {
	t.Parallel()
	_, err := resolveReleaseURL("", "", "")
	if err == nil {
		t.Fatal("unstamped fetch succeeded")
	}
	if !strings.Contains(err.Error(), "unstamped") || !strings.Contains(err.Error(), "--release-url") {
		t.Fatalf("error = %v", err)
	}
}

func TestResolveReleaseURLDevTagRefusesGitHub(t *testing.T) {
	t.Parallel()
	_, err := resolveReleaseURL("", "", "dev")
	if err == nil {
		t.Fatal("dev tag fetch succeeded")
	}
}

func TestReleaseBaseUsesProcessTag(t *testing.T) {
	t.Setenv(releaseURLEnv, "")
	prev := execenv.Tag
	execenv.Tag = "v9.9.9"
	t.Cleanup(func() { execenv.Tag = prev })
	got, err := (Options{}).releaseBase()
	if err != nil {
		t.Fatal(err)
	}
	if got != githubReleaseDownload+"/v9.9.9" {
		t.Fatalf("got %q", got)
	}
}
