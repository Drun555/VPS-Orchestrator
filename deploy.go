package main

import (
	"archive/tar"
	"bytes"
	"embed"
	"io/fs"
	"strings"

	"golang.org/x/crypto/ssh"
)

//go:embed deploy
var deploymentFiles embed.FS

func deploymentArchive() ([]byte, error) {
	var data bytes.Buffer
	archive := tar.NewWriter(&data)
	err := fs.WalkDir(deploymentFiles, "deploy", func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		content, err := deploymentFiles.ReadFile(path)
		if err != nil {
			return err
		}
		if err = archive.WriteHeader(&tar.Header{Name: strings.TrimPrefix(path, "deploy/"), Mode: 0600, Size: int64(len(content))}); err != nil {
			return err
		}
		_, err = archive.Write(content)
		return err
	})
	if err != nil {
		return nil, err
	}
	if err = archive.Close(); err != nil {
		return nil, err
	}
	return data.Bytes(), nil
}

func runEmbeddedInstaller(client *ssh.Client, script, args string, reporter *JobReporter) error {
	payload, err := deploymentArchive()
	if err != nil {
		return err
	}
	// No credentials or downloaded repository are required to transfer the payload.
	command := `set -eu; umask 077; stage=$(mktemp -d /tmp/vps-orchestrator.XXXXXX); trap 'rm -rf -- "$stage"' EXIT; tar -xf - -C "$stage"; bash "$stage/` + script + `" ` + args
	return runSSHCommandInput(client, command, payload, reporter)
}
