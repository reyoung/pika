package plugininstall

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
)

const manifestTemplate = `id = "pika-go"
name = "Pika-Go"
version = %s
min_herdr_version = "0.8.2"
description = "Long-running automatic optimization orchestrator"
platforms = ["linux", "macos"]

[[panes]]
id = "symphony"
title = "Daemon"
description = "Run the Pika-Go orchestration daemon"
placement = "split"
command = ["./pika-go", "daemon"]
`

type Options struct {
	Executable      string
	HerdrExecutable string
	InstallDir      string
	Version         string
	Stdin           io.Reader
	Stdout          io.Writer
	Stderr          io.Writer
}

type Result struct {
	InstallDir   string
	Executable   string
	ManifestPath string
}

func DefaultDir() (string, error) {
	if root := os.Getenv("XDG_DATA_HOME"); root != "" {
		if !filepath.IsAbs(root) {
			return "", errors.New("XDG_DATA_HOME must be an absolute path")
		}
		return filepath.Join(filepath.Clean(root), "pika-go", "plugin"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve user home: %w", err)
	}
	if !filepath.IsAbs(home) {
		return "", errors.New("user home must be an absolute path")
	}
	return filepath.Join(filepath.Clean(home), ".local", "share", "pika-go", "plugin"), nil
}

func Install(ctx context.Context, options Options) (Result, error) {
	if options.Executable == "" || !filepath.IsAbs(options.Executable) {
		return Result{}, errors.New("pika-go executable must be an absolute path")
	}
	if options.InstallDir == "" || !filepath.IsAbs(options.InstallDir) {
		return Result{}, errors.New("install directory must be an absolute path")
	}
	if options.HerdrExecutable == "" {
		return Result{}, errors.New("Herdr executable is required")
	}
	herdrExecutable, err := exec.LookPath(options.HerdrExecutable)
	if err != nil {
		return Result{}, fmt.Errorf("find Herdr executable: %w", err)
	}
	manifest, err := Manifest(options.Version)
	if err != nil {
		return Result{}, err
	}

	installDir := filepath.Clean(options.InstallDir)
	if err := os.MkdirAll(installDir, 0o755); err != nil {
		return Result{}, fmt.Errorf("create install directory: %w", err)
	}
	destinationExecutable := filepath.Join(installDir, "pika-go")
	if err := installExecutable(options.Executable, destinationExecutable); err != nil {
		return Result{}, err
	}
	manifestPath := filepath.Join(installDir, "herdr-plugin.toml")
	if err := replaceFile(manifestPath, 0o644, bytes.NewReader(manifest)); err != nil {
		return Result{}, fmt.Errorf("install plugin manifest: %w", err)
	}

	command := exec.CommandContext(ctx, herdrExecutable, "plugin", "link", installDir, "--enabled")
	command.Stdin = options.Stdin
	command.Stdout = options.Stdout
	command.Stderr = options.Stderr
	if err := command.Run(); err != nil {
		return Result{}, fmt.Errorf("register plugin with Herdr: %w", err)
	}
	return Result{InstallDir: installDir, Executable: destinationExecutable, ManifestPath: manifestPath}, nil
}

func Manifest(version string) ([]byte, error) {
	if version == "" {
		return nil, errors.New("version is required")
	}
	for _, character := range version {
		if (character >= 'a' && character <= 'z') ||
			(character >= 'A' && character <= 'Z') ||
			(character >= '0' && character <= '9') ||
			character == '.' || character == '-' || character == '_' || character == '+' {
			continue
		}
		return nil, fmt.Errorf("version contains unsupported character %q", character)
	}
	return []byte(fmt.Sprintf(manifestTemplate, `"`+version+`"`)), nil
}

func installExecutable(source, destination string) error {
	sourceInfo, err := os.Stat(source)
	if err != nil {
		return fmt.Errorf("inspect pika-go executable: %w", err)
	}
	if !sourceInfo.Mode().IsRegular() {
		return errors.New("pika-go executable must be a regular file")
	}
	if destinationInfo, statErr := os.Stat(destination); statErr == nil && os.SameFile(sourceInfo, destinationInfo) {
		if err := os.Chmod(destination, 0o755); err != nil {
			return fmt.Errorf("set installed executable permissions: %w", err)
		}
		return nil
	} else if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
		return fmt.Errorf("inspect installed executable: %w", statErr)
	}

	file, err := os.Open(source)
	if err != nil {
		return fmt.Errorf("open pika-go executable: %w", err)
	}
	defer file.Close()
	if err := replaceFile(destination, 0o755, file); err != nil {
		return fmt.Errorf("install pika-go executable: %w", err)
	}
	return nil
}

func replaceFile(path string, mode os.FileMode, contents io.Reader) error {
	directory := filepath.Dir(path)
	temporary, err := os.CreateTemp(directory, "."+filepath.Base(path)+".tmp-")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)

	if err := temporary.Chmod(mode); err != nil {
		temporary.Close()
		return err
	}
	if _, err := io.Copy(temporary, contents); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryPath, path)
}
