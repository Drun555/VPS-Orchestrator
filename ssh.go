package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/subtle"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
)

type SSHCredential struct {
	PrivateKey  string
	Passphrase  string
	Password    string
	UsePassword bool
}

func parsePrivateSigner(privateKey, passphrase string) (ssh.Signer, error) {
	privateKey = strings.TrimSpace(privateKey)
	if privateKey == "" {
		return nil, fmt.Errorf("private key is required")
	}
	var (
		signer ssh.Signer
		err    error
	)
	if passphrase != "" {
		signer, err = ssh.ParsePrivateKeyWithPassphrase([]byte(privateKey), []byte(passphrase))
	} else {
		signer, err = ssh.ParsePrivateKey([]byte(privateKey))
	}
	if err != nil {
		return nil, fmt.Errorf("parse private key: %w", err)
	}
	return signer, nil
}

func authorizedPublicKey(signer ssh.Signer) string {
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(signer.PublicKey())))
}

func validatePublicKeyMatches(signer ssh.Signer, publicKey string) error {
	parsed, _, _, _, err := ssh.ParseAuthorizedKey([]byte(strings.TrimSpace(publicKey)))
	if err != nil {
		return fmt.Errorf("parse public key: %w", err)
	}
	if subtle.ConstantTimeCompare(parsed.Marshal(), signer.PublicKey().Marshal()) != 1 {
		return fmt.Errorf("public key does not match the private key")
	}
	return nil
}

func dialSSH(ctx context.Context, host string, port int, credential SSHCredential, expectedFingerprint string) (*ssh.Client, string, error) {
	var auth []ssh.AuthMethod
	if !credential.UsePassword {
		signer, err := parsePrivateSigner(credential.PrivateKey, credential.Passphrase)
		if err != nil {
			return nil, "", err
		}
		auth = []ssh.AuthMethod{ssh.PublicKeys(signer)}
	}
	if credential.UsePassword {
		auth = []ssh.AuthMethod{
			ssh.Password(credential.Password),
			ssh.KeyboardInteractive(func(_ string, _ string, questions []string, _ []bool) ([]string, error) {
				answers := make([]string, len(questions))
				for index := range answers {
					answers[index] = credential.Password
				}
				return answers, nil
			}),
		}
	}

	observedFingerprint := ""
	configuration := &ssh.ClientConfig{
		User: "root", Auth: auth, Timeout: 15 * time.Second,
		HostKeyCallback: func(_ string, _ net.Addr, key ssh.PublicKey) error {
			observedFingerprint = ssh.FingerprintSHA256(key)
			if expectedFingerprint != "" && observedFingerprint != expectedFingerprint {
				return fmt.Errorf("SSH host key changed: expected %s, got %s", expectedFingerprint, observedFingerprint)
			}
			return nil
		},
	}

	address := net.JoinHostPort(host, fmt.Sprintf("%d", port))
	connection, err := (&net.Dialer{Timeout: 15 * time.Second}).DialContext(ctx, "tcp", address)
	if err != nil {
		return nil, "", fmt.Errorf("connect SSH %s: %w", address, err)
	}
	deadline := time.Now().Add(15 * time.Second)
	if limit, ok := ctx.Deadline(); ok && limit.Before(deadline) {
		deadline = limit
	}
	_ = connection.SetDeadline(deadline)
	stopCancel := context.AfterFunc(ctx, func() { _ = connection.Close() })
	clientConnection, channels, requests, err := ssh.NewClientConn(connection, address, configuration)
	if err != nil {
		stopCancel()
		_ = connection.Close()
		return nil, observedFingerprint, fmt.Errorf("authenticate SSH %s: %w", address, err)
	}
	_ = connection.SetDeadline(time.Time{})
	client := ssh.NewClient(clientConnection, channels, requests)
	go func() { _ = client.Wait(); stopCancel() }()
	return client, observedFingerprint, nil
}

func runSSHCommand(client *ssh.Client, command string, reporter *JobReporter) error {
	return runSSHCommandInput(client, command, nil, reporter)
}

func runSSHCommandInput(client *ssh.Client, command string, input []byte, reporter *JobReporter) error {
	session, err := client.NewSession()
	if err != nil {
		return fmt.Errorf("open SSH session: %w", err)
	}
	defer session.Close()
	session.Stdin = bytes.NewReader(input)
	stdout, err := session.StdoutPipe()
	if err != nil {
		return fmt.Errorf("open SSH stdout: %w", err)
	}
	stderr, err := session.StderrPipe()
	if err != nil {
		return fmt.Errorf("open SSH stderr: %w", err)
	}
	if err := session.Start(command); err != nil {
		return fmt.Errorf("start remote command: %w", err)
	}

	var wait sync.WaitGroup
	wait.Add(2)
	go streamSSHOutput(stdout, reporter, &wait)
	go streamSSHOutput(stderr, reporter, &wait)
	waitError := session.Wait()
	wait.Wait()
	if waitError != nil {
		return fmt.Errorf("remote command failed: %w", waitError)
	}
	return nil
}

func streamSSHOutput(reader io.Reader, reporter *JobReporter, wait *sync.WaitGroup) {
	defer wait.Done()
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	for scanner.Scan() {
		reporter.Log("%s", scanner.Text())
	}
	if err := scanner.Err(); err != nil {
		reporter.Warning("Ошибка чтения SSH-лога: %v", err)
	}
}

func readSSHFile(client *ssh.Client, path string) ([]byte, error) {
	session, err := client.NewSession()
	if err != nil {
		return nil, fmt.Errorf("open SSH session: %w", err)
	}
	defer session.Close()
	output, err := session.Output("/bin/cat -- " + shellQuote(path))
	if err != nil {
		return nil, fmt.Errorf("read remote file %s: %w", path, err)
	}
	return bytes.TrimSpace(output), nil
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}
