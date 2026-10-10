package main

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestProvisionFailureKeepsGeneratedIdentityAcrossRestart(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	listener.Close()
	app := newTestApp(t, State{Version: stateVersion, Settings: Settings{DuckDNSToken: "test-token"}})
	job, err := app.jobs.Start("provision", nil, func(r *JobReporter) error {
		return app.provisionServer(context.Background(), AddServerRequest{Address: address, DuckDNSURL: "test.duckdns.org", PasswordAuth: true, Password: "root-test-password"}, r)
	})
	if err != nil {
		t.Fatal(err)
	}
	waitForJob(t, job)
	state := app.store.Snapshot()
	if len(state.Servers) != 1 {
		t.Fatal("failed server lost")
	}
	server := state.Servers[0]
	if !server.Provisioning || server.Status != "setup_failed" || server.SSHPrivateKey == "" {
		t.Fatal("missing resumable identity")
	}
	signer, err := parsePrivateSigner(server.SSHPrivateKey, "")
	if err != nil {
		t.Fatal(err)
	}
	if err = validatePublicKeyMatches(signer, server.PublicKey); err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(state)
	if strings.Contains(string(data), "root-test-password") {
		t.Fatal("password persisted")
	}
	path := filepath.Join(t.TempDir(), "master.json")
	if err = os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	reopened, err := NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	app.store = reopened
	job, err = app.jobs.Start("resume", nil, func(r *JobReporter) error {
		return app.continueProvision(context.Background(), app.store.Snapshot().Servers[0], "", r)
	})
	if err != nil {
		t.Fatal(err)
	}
	waitForJob(t, job)
	if app.store.Snapshot().Servers[0].SSHPrivateKey != server.SSHPrivateKey {
		t.Fatal("retry rotated identity")
	}
	for _, event := range job.Snapshot().Events {
		if strings.Contains(event.Message, server.SSHPrivateKey) || strings.Contains(event.Message, "test-token") {
			t.Fatal("secret logged")
		}
	}
}
