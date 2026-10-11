package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type fakeManager struct {
	offline bool
	clients map[string][]remoteClient
	calls   map[string]int
}

func (f *fakeManager) Reconcile(_ context.Context, s Server, users []User, _ *JobReporter) (map[string]UserLink, error) {
	if f.calls == nil {
		f.calls = map[string]int{}
	}
	f.calls[s.ID]++
	if f.offline {
		return nil, errors.New("offline")
	}
	if f.clients == nil {
		f.clients = map[string][]remoteClient{}
	}
	clients, links, err := chooseClients(s, users, f.clients[s.ID], "public-key", "abcd")
	f.clients[s.ID] = clients
	return links, err
}
func runTestJob(t *testing.T, app *App, work func(*JobReporter) error) JobSnapshot {
	t.Helper()
	job, err := app.jobs.Start("test", nil, work)
	if err != nil {
		t.Fatal(err)
	}
	waitForJob(t, job)
	return job.Snapshot()
}

func TestAssignmentsReconcileAndRevokeAfterRestart(t *testing.T) {
	app := newTestApp(t, State{Version: stateVersion, Servers: []Server{{ID: "one", DuckDNSURL: "one.duckdns.org"}, {ID: "two", DuckDNSURL: "two.duckdns.org"}}})
	manager := &fakeManager{}
	app.servers = manager
	job := runTestJob(t, app, func(r *JobReporter) error {
		return app.createUser(context.Background(), AddUserRequest{Email: "Иван", ServerMode: "selected", ServerIDs: []string{"one"}}, r)
	})
	if job.Status != "success" {
		t.Fatal(job)
	}
	user := app.store.Snapshot().Users[0]
	token := user.SubscriptionToken
	if len(manager.clients["one"]) != 1 || len(manager.clients["two"]) != 0 {
		t.Fatal(manager.clients)
	}
	uuid := manager.clients["one"][0].ID
	manager.offline = true
	runTestJob(t, app, func(r *JobReporter) error {
		return app.updateUser(context.Background(), user.ID, UpdateUserRequest{ServerMode: "selected", ServerIDs: []string{}}, r)
	})
	state := app.store.Snapshot()
	user = state.Users[0]
	if user.Links["one"].Status != "revoking" {
		t.Fatal(user.Links)
	}
	if got := string(buildMihomoProvider(state, &user)); got != "proxies: []\n" {
		t.Fatal(got)
	}
	reopened, err := NewStore(app.store.path)
	if err != nil {
		t.Fatal(err)
	}
	app.store = reopened
	manager.offline = false
	runTestJob(t, app, func(r *JobReporter) error { return app.retryUser(context.Background(), user.ID, r) })
	if len(manager.clients["one"]) != 0 || len(app.store.Snapshot().Users[0].Links) != 0 {
		t.Fatal("revocation not completed")
	}
	runTestJob(t, app, func(r *JobReporter) error {
		return app.updateUser(context.Background(), user.ID, UpdateUserRequest{ServerMode: "all"}, r)
	})
	if len(manager.clients["two"]) != 1 {
		t.Fatal("all mode missing server")
	}
	if app.store.Snapshot().Users[0].SubscriptionToken != token {
		t.Fatal("token changed")
	}
	// Existing remote UUID wins, even if a stale local link contains something else.
	manager.clients["one"][0].ID = uuid
	runTestJob(t, app, func(r *JobReporter) error { return app.fullSync(context.Background(), r) })
	if app.store.Snapshot().Users[0].Links["one"].ClientID != uuid {
		t.Fatal("UUID not preserved")
	}
}

func TestAuthoritativeSyncAndNewServer(t *testing.T) {
	users := []User{{ID: "all", Email: "all", ServerMode: "all"}, {ID: "selected", Email: "selected", ServerMode: "selected", ServerIDs: []string{"one"}}}
	app := newTestApp(t, State{Version: stateVersion, Servers: []Server{{ID: "one"}, {ID: "new"}}, Users: users})
	manager := &fakeManager{clients: map[string][]remoteClient{"one": {{ID: "orphan", Email: "orphan"}, {ID: "existing", Email: "all"}, {ID: "duplicate", Email: "all"}}}}
	app.servers = manager
	runTestJob(t, app, func(r *JobReporter) error { return app.fullSync(context.Background(), r) })
	if len(manager.clients["one"]) != 2 || len(manager.clients["new"]) != 1 || manager.clients["one"][0].ID != "existing" {
		t.Fatal(manager.clients)
	}
	if manager.calls["one"] != 1 || manager.calls["new"] != 1 {
		t.Fatal("more than one reconcile per server")
	}
}

func TestFailurePersistsUserAndDeleteInvalidatesSubscription(t *testing.T) {
	app := newTestApp(t, State{Version: stateVersion, Servers: []Server{{ID: "one"}}})
	app.servers = &fakeManager{offline: true}
	job := runTestJob(t, app, func(r *JobReporter) error {
		return app.createUser(context.Background(), AddUserRequest{Email: "user"}, r)
	})
	user := app.store.Snapshot().Users[0]
	if job.Status != "success_with_warnings" || user.Links["one"].Status != "error" {
		t.Fatal(job, user)
	}
	runTestJob(t, app, func(r *JobReporter) error { return app.deleteUser(context.Background(), user.ID, r) })
	response := httptest.NewRecorder()
	app.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/subscribe/"+user.SubscriptionToken, nil))
	if response.Code != 404 || app.store.Snapshot().Servers[0].Status != "error" {
		t.Fatal("delete failed")
	}
}

func TestAssignmentHTTPValidation(t *testing.T) {
	app := newTestApp(t, State{Version: stateVersion, Users: []User{{ID: "user", ServerMode: "all"}}})
	for _, body := range []string{`{"server_mode":"selected","server_ids":["unknown"]}`, `{"server_mode":"invalid"}`, `{}`} {
		response := httptest.NewRecorder()
		app.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodPatch, "/api/users/user", strings.NewReader(body)))
		if response.Code != 400 {
			t.Fatalf("%s: %d", body, response.Code)
		}
	}
}

func TestStateV1MigrationPreservesSecretsAndTokens(t *testing.T) {
	path := filepath.Join(t.TempDir(), "master.json")
	original := []byte(`{"version":1,"servers":[{"id":"s","ssh_private_key":"PRIVATE","api_password":"obsolete"}],"users":[{"id":"u","subscription_token":"token","links":{"s":{"client_id":"uuid"}}}]}`)
	if err := os.WriteFile(path, original, 0600); err != nil {
		t.Fatal(err)
	}
	store, err := NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	state := store.Snapshot()
	if state.Version != 2 || state.Users[0].ServerMode != "all" || state.Users[0].SubscriptionToken != "token" || state.Users[0].Links["s"].ClientID != "uuid" || state.Servers[0].SSHPrivateKey != "PRIVATE" {
		t.Fatal("migration lost data")
	}
	backup, _ := os.ReadFile(path + ".v1.bak")
	if string(backup) != string(original) {
		t.Fatal("bad backup")
	}
	migrated, _ := os.ReadFile(path)
	if strings.Contains(string(migrated), "obsolete") {
		t.Fatal("legacy API password remains")
	}
	if _, err := NewStore(path); err != nil {
		t.Fatal("second startup", err)
	}
}

// Exercise the actual remote transaction shell against an isolated filesystem.
// Only service binaries are fakes; file writes, backups, permissions and renames are real.
type localRemote struct{ dir string }

func (r localRemote) Run(command string, input []byte) ([]byte, error) {
	paths := []string{"/usr/local/etc/xray", "/usr/local/bin/xray", "/etc/nginx/sites-available/vps-reality", "/etc/systemd/system/xray.service.d/vps-reality.conf", "/opt/vps-reality"}
	for _, path := range paths {
		command = strings.ReplaceAll(command, path, r.dir+path)
	}
	cmd := exec.Command("bash", "-c", command)
	cmd.Stdin = strings.NewReader(string(input))
	cmd.Env = append(os.Environ(), "PATH="+r.dir+"/bin:"+os.Getenv("PATH"), "FIXTURE="+r.dir)
	output, err := cmd.Output()
	return []byte(strings.ReplaceAll(string(output), r.dir, "")), err
}
func fixtureWrite(t *testing.T, root, path, content string, mode os.FileMode) {
	t.Helper()
	p := root + path
	if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
}
func newRemoteFixture(t *testing.T) (localRemote, Server, []User) {
	t.Helper()
	dir := t.TempDir()
	fixtureWrite(t, dir, xrayConfigPath, `{"log":{"loglevel":"warning"},"api":{"listen":"127.0.0.1:10085"},"inbounds":[{"tag":"vless-reality","settings":{"clients":[],"decryption":"none"},"streamSettings":{"security":"reality","realitySettings":{"privateKey":"keep-secret"}}}],"outbounds":[{"protocol":"freedom"}]}`, 0640)
	fixtureWrite(t, dir, runtimePath+"/clients.json", `{"clients":[{"id":"11111111-1111-4111-8111-111111111111","email":"user","uri":"old"}]}`, 0660)
	fixtureWrite(t, dir, runtimePath+"/clients.lock", "", 0660)
	fixtureWrite(t, dir, runtimePath+"/reality-public-key", "public", 0600)
	fixtureWrite(t, dir, runtimePath+"/reality-short-id", "abcd", 0600)
	fixtureWrite(t, dir, nginxSitePath, "server {\n location = /register {\n limit_except POST { deny all; }\n fastcgi_pass unix:/run/fcgiwrap.socket;\n }\n location / { return 404; }\n}\n", 0644)
	fixtureWrite(t, dir, dropinPath, "[Unit]\nAfter=nginx.service\n[Service]\nExecStartPost=/opt/vps-reality/scripts/sync-users.sh\n", 0644)
	fixtureWrite(t, dir, "/usr/local/bin/xray", "#!/bin/sh\ntest ! -f \"$FIXTURE/reject-config\"\n", 0700)
	fixtureWrite(t, dir, "/bin/nginx", "#!/bin/sh\nexit 0\n", 0700)
	fixtureWrite(t, dir, "/bin/systemctl", `#!/bin/sh
printf '%s\n' "$*" >> "$FIXTURE/service-log"
if [ "$*" = 'restart xray' ] && [ -f "$FIXTURE/fail-restart" ]; then rm "$FIXTURE/fail-restart"; exit 1; fi
`, 0700)
	return localRemote{dir}, Server{ID: "s", DuckDNSURL: "one.duckdns.org"}, []User{{ID: "u", Email: "user", ServerMode: "all"}}
}
func fixtureReporter() *JobReporter {
	return &JobReporter{job: &Job{subscribers: map[chan JobEvent]struct{}{}}}
}
func TestRemoteMigrationIdempotenceAndPreservation(t *testing.T) {
	remote, server, users := newRemoteFixture(t)
	links, err := reconcileRemote(remote, server, users, fixtureReporter())
	if err != nil {
		t.Fatal(err)
	}
	if links["u"].ClientID != "11111111-1111-4111-8111-111111111111" {
		t.Fatal(links)
	}
	data, _ := os.ReadFile(remote.dir + xrayConfigPath)
	if !strings.Contains(string(data), "keep-secret") || !strings.Contains(string(data), `"StatsService"`) {
		t.Fatal(string(data))
	}
	site, _ := os.ReadFile(remote.dir + nginxSitePath)
	if strings.Contains(string(site), "fastcgi") || !strings.Contains(string(site), "return 404") {
		t.Fatal(string(site))
	}
	if _, err := reconcileRemote(remote, server, users, fixtureReporter()); err != nil {
		t.Fatal(err)
	}
	log, _ := os.ReadFile(remote.dir + "/service-log")
	if strings.Count(string(log), "restart xray") != 1 {
		t.Fatal("unnecessary restart", string(log))
	}
	var cfg map[string]any
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatal(err)
	}
}
func TestRemoteRollbackAndRetry(t *testing.T) {
	remote, server, users := newRemoteFixture(t)
	before, _ := os.ReadFile(remote.dir + xrayConfigPath)
	siteBefore, _ := os.ReadFile(remote.dir + nginxSitePath)
	fixtureWrite(t, remote.dir, "/fail-restart", "", 0600)
	if _, err := reconcileRemote(remote, server, users, fixtureReporter()); err == nil {
		t.Fatal("expected failed restart")
	}
	after, _ := os.ReadFile(remote.dir + xrayConfigPath)
	siteAfter, _ := os.ReadFile(remote.dir + nginxSitePath)
	if string(before) != string(after) || string(siteBefore) != string(siteAfter) {
		t.Fatal("rollback lost original configuration")
	}
	if _, err := os.Stat(remote.dir + runtimePath + "/installation-version"); !os.IsNotExist(err) {
		t.Fatal("failed migration marked complete")
	}
	if _, err := reconcileRemote(remote, server, users, fixtureReporter()); err != nil {
		t.Fatal(err)
	}
}
func TestRemoteValidationFailureDoesNotModifyLiveConfig(t *testing.T) {
	remote, server, users := newRemoteFixture(t)
	before, _ := os.ReadFile(remote.dir + xrayConfigPath)
	fixtureWrite(t, remote.dir, "/reject-config", "", 0600)
	if _, err := reconcileRemote(remote, server, users, fixtureReporter()); err == nil {
		t.Fatal("expected validation error")
	}
	after, _ := os.ReadFile(remote.dir + xrayConfigPath)
	if string(before) != string(after) {
		t.Fatal("live config changed")
	}
}

func TestChooseClientsQuotesUntrustedName(t *testing.T) {
	name := "user'; $(touch /tmp/unsafe)\nимя"
	_, links, err := chooseClients(Server{ID: "s", DuckDNSURL: "one.duckdns.org"}, []User{{ID: "u", Email: name}}, nil, "pub", "abcd")
	if err != nil {
		t.Fatal(err)
	}
	proxy, err := parseVLESSReality(links["u"].URI, "test")
	if err != nil {
		t.Fatal(err)
	}
	_ = proxy
}

func TestForgetServerCleansAssignments(t *testing.T) {
	app := newTestApp(t, State{Version: stateVersion, Servers: []Server{{ID: "one"}}, Users: []User{{ID: "u", ServerMode: "selected", ServerIDs: []string{"one"}, Links: map[string]UserLink{"one": {}}}}})
	if err := app.forgetServer("one"); err != nil {
		t.Fatal(err)
	}
	u := app.store.Snapshot().Users[0]
	if len(u.ServerIDs) != 0 || len(u.Links) != 0 {
		t.Fatal(u)
	}
}

func TestSSHHandshakeCancellation(t *testing.T) {
	// An already cancelled context must not begin authentication.
	key, _ := testPrivateKey(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	_, _, err := dialSSH(ctx, "127.0.0.1", 1, SSHCredential{PrivateKey: key}, "")
	if err == nil || time.Since(start) > time.Second {
		t.Fatal("cancellation ignored")
	}
}

func TestRemoteInterruptedTransactionRecoveredOnNextAttempt(t *testing.T) {
	remote, server, users := newRemoteFixture(t)
	// Reproduce an interrupted transaction: backups and pending marker exist, live config is damaged.
	original, err := os.ReadFile(remote.dir + xrayConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	site, _ := os.ReadFile(remote.dir + nginxSitePath)
	dropin, _ := os.ReadFile(remote.dir + dropinPath)
	for path, data := range map[string][]byte{"xray": original, "nginx": site, "dropin": dropin, "pending": {}, "migration": {}} {
		fixtureWrite(t, remote.dir, transactionPath+"/"+path, string(data), 0600)
	}
	fixtureWrite(t, remote.dir, xrayConfigPath, "incomplete write", 0600)
	if _, err := reconcileRemote(remote, server, users, fixtureReporter()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(remote.dir + transactionPath + "/pending"); !os.IsNotExist(err) {
		t.Fatal("pending marker was not cleared")
	}
	after, _ := os.ReadFile(remote.dir + xrayConfigPath)
	if !strings.Contains(string(after), "keep-secret") {
		t.Fatal("recovery lost original key")
	}
}

func TestPinnedHostMismatchRejected(t *testing.T) {
	private, signer := testPrivateKey(t)
	listener, _, closeServer := startTestSSHServer(t, "unused", signer.PublicKey())
	defer closeServer()
	host, port, _, err := parseSSHAddress(listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	if client, _, err := dialSSH(context.Background(), host, port, SSHCredential{PrivateKey: private}, "SHA256:wrong"); err == nil {
		client.Close()
		t.Fatal("changed host key accepted")
	}
}
