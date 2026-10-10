package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

func TestPrivateKeyAndPublicKeyMatching(t *testing.T) {
	privatePEM, signer := testPrivateKey(t)
	parsed, err := parsePrivateSigner(privatePEM, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := validatePublicKeyMatches(parsed, authorizedPublicKey(signer)); err != nil {
		t.Fatal(err)
	}
	_, other := testPrivateKey(t)
	if err := validatePublicKeyMatches(parsed, authorizedPublicKey(other)); err == nil {
		t.Fatal("expected mismatched public key error")
	}
}

func TestEncryptedPrivateKey(t *testing.T) {
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	block, err := ssh.MarshalPrivateKeyWithPassphrase(private, "master-test", []byte("correct horse"))
	if err != nil {
		t.Fatal(err)
	}
	encoded := string(pem.EncodeToMemory(block))
	if _, err := parsePrivateSigner(encoded, "correct horse"); err != nil {
		t.Fatal(err)
	}
	if _, err := parsePrivateSigner(encoded, "wrong"); err == nil {
		t.Fatal("expected wrong passphrase error")
	}
}

func TestDialSSHPasswordAndTOFU(t *testing.T) {
	listener, fingerprint, closeServer := startTestSSHServer(t, "secret", nil)
	defer closeServer()
	host, portText, _ := net.SplitHostPort(listener.Addr().String())
	var port int
	_, _ = fmtSscanf(portText, &port)
	client, observed, err := dialSSH(context.Background(), host, port, SSHCredential{Password: "secret", UsePassword: true}, "")
	if err != nil {
		t.Fatal(err)
	}
	client.Close()
	if observed != fingerprint {
		t.Fatalf("got fingerprint %s, want %s", observed, fingerprint)
	}
}

func TestDialSSHPrivateKeyAndPinnedHost(t *testing.T) {
	privatePEM, signer := testPrivateKey(t)
	listener, fingerprint, closeServer := startTestSSHServer(t, "unused", signer.PublicKey())
	defer closeServer()
	host, portText, _ := net.SplitHostPort(listener.Addr().String())
	var port int
	_, _ = fmtSscanf(portText, &port)
	client, observed, err := dialSSH(context.Background(), host, port, SSHCredential{PrivateKey: privatePEM}, fingerprint)
	if err != nil {
		t.Fatal(err)
	}
	client.Close()
	if observed != fingerprint {
		t.Fatalf("got fingerprint %s, want %s", observed, fingerprint)
	}
}

func TestRunSSHCommandStreamsOutputAndFails(t *testing.T) {
	privatePEM, signer := testPrivateKey(t)
	listener, fingerprint, closeServer := startExecSSHServer(t, signer.PublicKey(), 17)
	defer closeServer()
	host, portText, _ := net.SplitHostPort(listener.Addr().String())
	var port int
	_, _ = fmtSscanf(portText, &port)
	manager := NewJobManager()
	job, err := manager.Start("ssh", nil, func(reporter *JobReporter) error {
		client, _, dialErr := dialSSH(context.Background(), host, port, SSHCredential{PrivateKey: privatePEM}, fingerprint)
		if dialErr != nil {
			return dialErr
		}
		defer client.Close()
		return runSSHCommand(client, "false", reporter)
	})
	if err != nil {
		t.Fatal(err)
	}
	waitForJob(t, job)
	snapshot := job.Snapshot()
	if snapshot.Status != "error" {
		t.Fatalf("unexpected job status: %+v", snapshot)
	}
	foundOutput := false
	for _, event := range snapshot.Events {
		if event.Message == "remote output" {
			foundOutput = true
		}
	}
	if !foundOutput {
		t.Fatalf("remote output was not streamed: %+v", snapshot.Events)
	}
}

func TestCleanupFailedServerOverSSH(t *testing.T) {
	privatePEM, signer := testPrivateKey(t)
	listener, _, closeServer := startExecSSHServer(t, signer.PublicKey(), 0)
	defer closeServer()
	host, portText, _ := net.SplitHostPort(listener.Addr().String())
	app := newTestApp(t, State{Version: stateVersion, Servers: []Server{}, Users: []User{}})
	manager := NewJobManager()
	job, err := manager.Start("cleanup", nil, func(reporter *JobReporter) error {
		return app.cleanupFailedServer(context.Background(), AddServerRequest{
			Address: net.JoinHostPort(host, portText), PrivateKey: privatePEM,
			DuckDNSURL: "cleanup.duckdns.org",
		}, reporter)
	})
	if err != nil {
		t.Fatal(err)
	}
	waitForJob(t, job)
	if snapshot := job.Snapshot(); snapshot.Status != "success" {
		t.Fatalf("cleanup failed: %+v", snapshot)
	}
}

func testPrivateKey(t *testing.T) (string, ssh.Signer) {
	t.Helper()
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(private)
	if err != nil {
		t.Fatal(err)
	}
	value := string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
	signer, err := ssh.NewSignerFromKey(private)
	if err != nil {
		t.Fatal(err)
	}
	return value, signer
}

func startTestSSHServer(t *testing.T, password string, allowed ssh.PublicKey) (net.Listener, string, func()) {
	t.Helper()
	_, hostSigner := testPrivateKey(t)
	configuration := &ssh.ServerConfig{
		PasswordCallback: func(metadata ssh.ConnMetadata, provided []byte) (*ssh.Permissions, error) {
			if metadata.User() == "root" && string(provided) == password {
				return nil, nil
			}
			return nil, assertError("denied")
		},
		PublicKeyCallback: func(metadata ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			if metadata.User() == "root" && allowed != nil && string(key.Marshal()) == string(allowed.Marshal()) {
				return nil, nil
			}
			return nil, assertError("denied")
		},
	}
	configuration.AddHostKey(hostSigner)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	go func() {
		for {
			connection, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}
			go func() {
				serverConnection, channels, requests, handshakeErr := ssh.NewServerConn(connection, configuration)
				if handshakeErr != nil {
					_ = connection.Close()
					return
				}
				go ssh.DiscardRequests(requests)
				go func() {
					for channel := range channels {
						_ = channel.Reject(ssh.UnknownChannelType, "not supported")
					}
				}()
				select {
				case <-stop:
				case <-time.After(3 * time.Second):
				}
				_ = serverConnection.Close()
			}()
		}
	}()
	return listener, ssh.FingerprintSHA256(hostSigner.PublicKey()), func() { close(stop); _ = listener.Close() }
}

func startExecSSHServer(t *testing.T, allowed ssh.PublicKey, exitStatus uint32) (net.Listener, string, func()) {
	t.Helper()
	_, hostSigner := testPrivateKey(t)
	configuration := &ssh.ServerConfig{PublicKeyCallback: func(metadata ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
		if metadata.User() == "root" && string(key.Marshal()) == string(allowed.Marshal()) {
			return nil, nil
		}
		return nil, assertError("denied")
	}}
	configuration.AddHostKey(hostSigner)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		connection, acceptErr := listener.Accept()
		if acceptErr != nil {
			return
		}
		serverConnection, channels, requests, handshakeErr := ssh.NewServerConn(connection, configuration)
		if handshakeErr != nil {
			_ = connection.Close()
			return
		}
		defer serverConnection.Close()
		go ssh.DiscardRequests(requests)
		for newChannel := range channels {
			if newChannel.ChannelType() != "session" {
				_ = newChannel.Reject(ssh.UnknownChannelType, "unsupported")
				continue
			}
			channel, channelRequests, acceptErr := newChannel.Accept()
			if acceptErr != nil {
				continue
			}
			go func() {
				defer channel.Close()
				for request := range channelRequests {
					if request.Type != "exec" {
						_ = request.Reply(false, nil)
						continue
					}
					_ = request.Reply(true, nil)
					_, _ = io.Copy(io.Discard, channel)
					_, _ = channel.Write([]byte("remote output\n"))
					_, _ = channel.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{exitStatus}))
					return
				}
			}()
		}
	}()
	return listener, ssh.FingerprintSHA256(hostSigner.PublicKey()), func() { _ = listener.Close() }
}

func fmtSscanf(value string, target *int) (int, error) {
	return fmt.Sscanf(value, "%d", target)
}
