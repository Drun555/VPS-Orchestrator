package main

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestReconfigureFailureKeepsAssignmentsAndResumableIdentity(t *testing.T) {
	key, _ := testPrivateKey(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	listener.Close()
	created := time.Now().Add(-time.Hour).UTC()
	app := newTestApp(t, State{Version: stateVersion, Settings: Settings{DuckDNSToken: "shared-secret"},
		Servers: []Server{{ID: "one", Address: address, DuckDNSURL: "one.duckdns.org", SSHPrivateKey: key, SSHHostFingerprint: "old-host", SharedDuckDNS: true, CreatedAt: created, Status: "ready"}},
		Users:   []User{{ID: "user", ServerMode: "selected", ServerIDs: []string{"one"}, RecommendedServerID: "one", SubscriptionToken: "subscription", Links: map[string]UserLink{"one": {ClientID: "uuid", URI: "old-uri", Status: "ready"}}}},
	})
	input := AddServerRequest{Address: address, DuckDNSURL: "new.duckdns.org", DisplayName: "Rebuilt", SiteTemplate: "syncthing", Private: true, PasswordAuth: true, Password: "root-secret"}
	body, _ := json.Marshal(input)
	response := httptest.NewRecorder()
	app.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/servers/one/reconfigure", strings.NewReader(string(body))))
	if response.Code != http.StatusAccepted {
		t.Fatalf("reconfigure: %d %s", response.Code, response.Body.String())
	}
	var result struct {
		JobID string `json:"job_id"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	job, ok := app.jobs.Get(result.JobID)
	if !ok {
		t.Fatal("job missing")
	}
	waitForJob(t, job)
	state := app.store.Snapshot()
	server, user := state.Servers[0], state.Users[0]
	if len(state.Servers) != 1 || server.ID != "one" || !server.CreatedAt.Equal(created) || server.SSHPrivateKey != key || server.SSHHostFingerprint != "" || server.SetupAttemptID == "" || !server.Provisioning || server.Status != "setup_failed" {
		t.Fatal("reconfiguration lost the identity or cannot resume")
	}
	if !server.Private || server.DisplayName != "Rebuilt" || server.DuckDNSURL != "new.duckdns.org" || server.SiteTemplate != "syncthing" {
		t.Fatal("form settings were not applied")
	}
	if user.ServerIDs[0] != "one" || user.RecommendedServerID != "one" || user.SubscriptionToken != "subscription" || user.Links["one"].ClientID != "uuid" || user.Links["one"].Status != "pending" || user.Links["one"].URI != "" {
		t.Fatal("assignments or UUID lost, or stale connection still published")
	}
	raw, _ := json.Marshal(state)
	if strings.Contains(string(raw), "root-secret") {
		t.Fatal("root password persisted")
	}
	for _, event := range job.Snapshot().Events {
		if strings.Contains(event.Message, "root-secret") || strings.Contains(event.Message, "shared-secret") || strings.Contains(event.Message, key) {
			t.Fatal("secret logged")
		}
	}
	// A normal retry must not create another attempt or rotate the key.
	runTestJob(t, app, func(r *JobReporter) error { return app.continueProvision(context.Background(), server, "", r) })
	if app.store.Snapshot().Servers[0].SetupAttemptID != server.SetupAttemptID {
		t.Fatal("retry replaced setup attempt")
	}
}

func TestPrepareReconfigurationValidationAndCredentials(t *testing.T) {
	key, _ := testPrivateKey(t)
	newKey, signer := testPrivateKey(t)
	state := State{Servers: []Server{
		{ID: "one", Address: "192.0.2.1:22", DuckDNSURL: "one.duckdns.org", SSHPrivateKey: key, DuckDNSToken: "legacy-token"},
		{ID: "two", Address: "192.0.2.2:22", DuckDNSURL: "two.duckdns.org"},
	}}
	valid := AddServerRequest{Address: "192.0.2.1", DuckDNSURL: "one.duckdns.org", SiteTemplate: "file-hosting"}
	for _, tc := range []struct {
		name   string
		change func(*AddServerRequest)
	}{
		{"address collision", func(r *AddServerRequest) { r.Address = "192.0.2.2:22" }},
		{"domain collision", func(r *AddServerRequest) { r.DuckDNSURL = "two.duckdns.org" }},
		{"invalid address", func(r *AddServerRequest) { r.Address = "" }},
		{"invalid domain", func(r *AddServerRequest) { r.DuckDNSURL = "example.com" }},
		{"invalid template", func(r *AddServerRequest) { r.SiteTemplate = "unknown" }},
		{"missing password", func(r *AddServerRequest) { r.PasswordAuth = true }},
		{"bad key", func(r *AddServerRequest) { r.PrivateKey = "invalid" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input := valid
			tc.change(&input)
			if _, err := prepareReconfiguration(state, "one", input, ""); err == nil {
				t.Fatal("invalid request accepted")
			}
			if state.Servers[0].SetupAttemptID != "" {
				t.Fatal("validation mutated state")
			}
		})
	}
	server, err := prepareReconfiguration(state, "one", valid, "")
	if err != nil || server.SSHPrivateKey != key || server.DuckDNSToken != "legacy-token" || server.SharedDuckDNS {
		t.Fatal("stored credentials not reused", err)
	}
	valid.PrivateKey = newKey
	server, err = prepareReconfiguration(state, "one", valid, "")
	if err != nil || server.PublicKey != authorizedPublicKey(signer) {
		t.Fatal("replacement key not used", err)
	}
	response := httptest.NewRecorder()
	app := newTestApp(t, state)
	app.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/servers/missing/reconfigure", strings.NewReader(`{}`)))
	if response.Code != http.StatusNotFound {
		t.Fatal("missing server accepted")
	}
}

func TestOnlyExplicitReconfigurationAcceptsRebuiltHostKey(t *testing.T) {
	key, signer := testPrivateKey(t)
	listener, fingerprint, closeServer := startTestSSHServer(t, "unused", signer.PublicKey())
	defer closeServer()
	host, portText, _ := net.SplitHostPort(listener.Addr().String())
	var port int
	fmtSscanf(portText, &port)
	app := newTestApp(t, State{Version: stateVersion, Servers: []Server{{
		ID: "one", Address: listener.Addr().String(), SSHHost: host, SSHPort: port,
		DuckDNSURL: "one.duckdns.org", DuckDNSToken: "token", SSHPrivateKey: key,
		PublicKey: authorizedPublicKey(signer), SSHHostFingerprint: "old-host-key", Provisioning: true,
	}}})
	job := runTestJob(t, app, func(r *JobReporter) error {
		return app.continueProvision(context.Background(), app.store.Snapshot().Servers[0], "", r)
	})
	if !strings.Contains(job.Error, "SSH host key changed") || app.store.Snapshot().Servers[0].SSHHostFingerprint != "old-host-key" {
		t.Fatal("normal resume accepted a changed host key")
	}
	job = runTestJob(t, app, func(r *JobReporter) error {
		return app.reconfigureServer(context.Background(), "one", AddServerRequest{Address: listener.Addr().String(), DuckDNSURL: "one.duckdns.org"}, r)
	})
	// This SSH fixture deliberately rejects exec channels. Reaching that step
	// proves authentication succeeded and the new fingerprint was pinned.
	if !strings.Contains(job.Error, "open SSH session") || app.store.Snapshot().Servers[0].SSHHostFingerprint != fingerprint {
		t.Fatalf("explicit rebuild failed to pin new host key: %s", job.Error)
	}
}
