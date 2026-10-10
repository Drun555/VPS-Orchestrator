package main

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"
)

// Opt-in deployment acceptance check. Never runs in CI or the normal test suite.
// VPS_LIVE_STATE is an operator-provided state copy; VPS_LIVE_MIGRATE must be one exact server ID.
func TestLiveServerAcceptance(t *testing.T) {
	path := os.Getenv("VPS_LIVE_STATE")
	if path == "" {
		t.Skip("live state not configured")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var state State
	if err = json.Unmarshal(data, &state); err != nil {
		t.Fatal(err)
	}
	for _, server := range state.Servers {
		t.Run(server.ID, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
			defer cancel()
			client, _, err := dialSSH(ctx, server.SSHHost, server.SSHPort, SSHCredential{PrivateKey: server.SSHPrivateKey, Passphrase: server.SSHPassphrase}, server.SSHHostFingerprint)
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			remote := sshRemote{client}
			config, err := readRemote(remote, xrayConfigPath)
			if err != nil {
				t.Fatal(err)
			}
			doc, err := parseXray(config)
			if err != nil {
				t.Fatal(err)
			}
			version, err := remote.Run("if [ -f "+runtimePath+"/installation-version ]; then cat "+runtimePath+"/installation-version; else printf 1; fi", nil)
			if err != nil {
				t.Fatal(err)
			}
			health, err := remote.Run("systemctl is-active xray nginx; uname -m", nil)
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("installation=%s config_clients=%d services=%s", version, len(doc.clients), health)
			if os.Getenv("VPS_LIVE_MIGRATE") != server.ID {
				return
			}
			links, err := (SSHServerManager{}).Reconcile(ctx, server, state.Users, fixtureReporter())
			if err != nil {
				t.Fatal(err)
			}
			for _, user := range state.Users {
				if expected := user.Links[server.ID].ClientID; expected != "" && links[user.ID].ClientID != expected {
					t.Fatal("existing UUID changed")
				}
			}
			after, err := readRemote(remote, xrayConfigPath)
			if err != nil {
				t.Fatal(err)
			}
			newDoc, err := parseXray(after)
			if err != nil {
				t.Fatal(err)
			}
			if len(newDoc.clients) != len(links) {
				t.Fatal("persisted clients do not match")
			}
			if _, err = remote.Run("set -eu; systemctl is-active --quiet xray; systemctl is-active --quiet nginx; test ! -e /opt/vps-reality/scripts/sync-users.sh; test ! -e /opt/vps-reality/cgi/register.sh", nil); err != nil {
				t.Fatal(err)
			}
			// A second reconcile must preserve the service process (no redundant restart).
			pid, err := remote.Run("systemctl show xray --property=MainPID --value", nil)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = (SSHServerManager{}).Reconcile(ctx, server, state.Users, fixtureReporter()); err != nil {
				t.Fatal(err)
			}
			nextPID, err := remote.Run("systemctl show xray --property=MainPID --value", nil)
			if err != nil || string(pid) != string(nextPID) {
				t.Fatal("idempotent reconcile restarted Xray")
			}
			t.Logf("migration verified: %d users, UUIDs preserved, services active, repeat did not restart", len(links))
		})
	}
}
