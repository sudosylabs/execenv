package bake

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/sudosylabs/execenv"
)

func defaultSteps() steps {
	return steps{exportFS: exportContainer, packFS: packExt4}
}

func exportContainer(ctx context.Context, image, dockerfile, dest string) (string, error) {
	cli, err := lookContainerCLI()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return "", err
	}
	ref := image
	if dockerfile != "" {
		ref = "execenv-bake:local"
		dir := filepath.Dir(dockerfile)
		if err := runCmd(ctx, cli, "build", "-f", dockerfile, "-t", ref, dir); err != nil {
			return "", err
		}
	} else if ref == "" {
		return "", execenv.ErrInvalid
	}
	name := "execenv-bake-export"
	_ = runCmd(ctx, cli, "rm", "-f", name)
	if err := runCmd(ctx, cli, "create", "--name", name, ref); err != nil {
		return "", err
	}
	defer func() { _ = runCmd(context.Background(), cli, "rm", "-f", name) }()
	archive := dest + ".tar"
	if err := runCmd(ctx, cli, "export", name, "-o", archive); err != nil {
		return "", err
	}
	defer os.Remove(archive)
	if err := unpackTar(ctx, archive, dest); err != nil {
		return "", err
	}
	return inspectImage(ctx, cli, ref, dockerfile, image), nil
}

func packExt4(ctx context.Context, staging, dest string, size int64) error {
	mkfs, err := exec.LookPath("mkfs.ext4")
	if err != nil {
		return execenv.ErrUnavailable
	}
	if err := truncate(dest, size); err != nil {
		return err
	}
	return runCmd(ctx, mkfs, "-q", "-F", "-d", staging, dest)
}

func unpackTar(ctx context.Context, archive, dest string) error {
	tar, err := exec.LookPath("tar")
	if err != nil {
		return execenv.ErrUnavailable
	}
	return runCmd(ctx, tar, "-xf", archive, "-C", dest)
}

func truncate(path string, size int64) error {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_RDWR, 0o644)
	if err != nil {
		return err
	}
	err = file.Truncate(size)
	_ = file.Close()
	return err
}

func inspectImage(ctx context.Context, cli, ref, dockerfile, image string) string {
	cmd := exec.CommandContext(ctx, cli, "image", "inspect", "--format", "{{.Id}}", ref)
	out, err := cmd.Output()
	if err == nil {
		id := strings.TrimSpace(string(out))
		if id != "" {
			return id
		}
	}
	if dockerfile != "" {
		return dockerfile
	}
	return image
}

func lookContainerCLI() (string, error) {
	for _, name := range []string{"docker", "podman"} {
		path, err := exec.LookPath(name)
		if err == nil {
			return path, nil
		}
	}
	return "", execenv.ErrUnavailable
}

func runCmd(ctx context.Context, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdin = nil
	// Combined output is only used to decide success. It is not logged.
	out, err := cmd.CombinedOutput()
	_ = out
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("%s: %w", name, execenv.ErrUnavailable)
	}
	return nil
}
