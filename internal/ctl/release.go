package ctl

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/sudosylabs/execenv"
)

var httpClient = &http.Client{Timeout: 30 * time.Minute}

const (
	githubReleaseDownload = "https://github.com/sudosylabs/execenv/releases/download"
	releaseURLEnv         = "EXECENV_RELEASE_URL"
)

func resolveReleaseURL(flag, env, tag string) (string, error) {
	if flag != "" {
		return strings.TrimRight(flag, "/"), nil
	}
	if env != "" {
		return strings.TrimRight(env, "/"), nil
	}
	if tag == "" || tag == "dev" {
		return "", wrap("release", fmt.Errorf("unstamped binary; pass --release-url or EXECENV_RELEASE_URL"))
	}
	return githubReleaseDownload + "/" + tag, nil
}

func (o Options) releaseBase() (string, error) {
	return resolveReleaseURL(o.ReleaseURL, os.Getenv(releaseURLEnv), execenv.Tag)
}

func fetchURL(url, dest string) error {
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return wrap("fetch", err)
	}
	resp, err := httpClient.Get(url)
	if err != nil {
		return wrap("fetch", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return wrap("fetch", fmt.Errorf("%s: %s", filepath.Base(url), resp.Status))
	}
	tmp := dest + ".tmp"
	file, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return wrap("fetch", err)
	}
	_, err = io.Copy(file, resp.Body)
	closeErr := file.Close()
	if err != nil {
		_ = os.Remove(tmp)
		return wrap("fetch", err)
	}
	if closeErr != nil {
		_ = os.Remove(tmp)
		return wrap("fetch", closeErr)
	}
	if err := os.Rename(tmp, dest); err != nil {
		_ = os.Remove(tmp)
		return wrap("fetch", err)
	}
	return nil
}

func fetchBytes(url string) ([]byte, error) {
	resp, err := httpClient.Get(url)
	if err != nil {
		return nil, wrap("fetch", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, wrap("fetch", fmt.Errorf("%s: %s", filepath.Base(url), resp.Status))
	}
	return io.ReadAll(resp.Body)
}
