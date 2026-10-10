package main

import (
	"context"
	"strings"
	"testing"
)

func TestPrivateServerAssignmentsAndRevocation(t *testing.T) {
	private := Server{ID: "private", Private: true, DuckDNSURL: "private.duckdns.org"}
	public := Server{ID: "public", DuckDNSURL: "public.duckdns.org"}
	stale := User{ID: "existing", Email: "existing", ServerMode: "all", SubscriptionToken: "keep-token", Links: map[string]UserLink{"private": {ServerID: "private", Status: "ready", URI: clientURI(private, "11111111-1111-4111-8111-111111111111", "existing", "key", "abcd")}}}
	selected := User{ID: "selected", Email: "selected", ServerMode: "selected", ServerIDs: []string{"private"}}
	app := newTestApp(t, State{Version: stateVersion, Servers: []Server{public, private}, Users: []User{stale, selected}})
	manager := &fakeManager{}
	app.servers = manager
	ctx := context.Background()
	run := func(work func(*JobReporter) error) {
		t.Helper()
		job := runTestJob(t, app, work)
		if job.Status == "error" {
			t.Fatal(job)
		}
	}
	if got := string(buildMihomoProvider(app.store.Snapshot(), &stale)); got != "proxies: []\n" {
		t.Fatalf("stale private link leaked: %s", got)
	}
	for _, mode := range []string{"all", "", "invalid"} {
		if userAllowsServer(User{ServerMode: mode, ServerIDs: []string{"private"}}, private) {
			t.Fatalf("private allowed in mode %q", mode)
		}
	}
	run(func(r *JobReporter) error { return app.fullSync(ctx, r) })
	if len(manager.clients["private"]) != 1 || manager.clients["private"][0].Email != "selected" {
		t.Fatal("private clients were not explicitly assigned")
	}
	calls := manager.calls["private"]
	run(func(r *JobReporter) error {
		return app.createUser(ctx, AddUserRequest{Email: "new-all", ServerMode: "all"}, r)
	})
	if manager.calls["private"] != calls {
		t.Fatal("all user automatically synced to private server")
	}
	run(func(r *JobReporter) error {
		return app.updateUser(ctx, "existing", UpdateUserRequest{ServerMode: "selected", ServerIDs: []string{"private"}}, r)
	})
	state := app.store.Snapshot()
	user, _ := findUser(&state, "existing")
	if !strings.Contains(string(buildMihomoProvider(state, user)), "private.duckdns.org") {
		t.Fatal("explicit assignment missing from subscription")
	}
	manager.offline = true
	run(func(r *JobReporter) error {
		return app.updateUser(ctx, "existing", UpdateUserRequest{ServerMode: "all"}, r)
	})
	state = app.store.Snapshot()
	user, _ = findUser(&state, "existing")
	if user.Links["private"].Status != "revoking" {
		t.Fatal("private revocation not tracked")
	}
	if strings.Contains(string(buildMihomoProvider(state, user)), "private.duckdns.org") {
		t.Fatal("all mode retained private subscription")
	}
	reopened, err := NewStore(app.store.path)
	if err != nil {
		t.Fatal(err)
	}
	app.store = reopened
	manager.offline = false
	run(func(r *JobReporter) error { return app.retryUser(ctx, "existing", r) })
	if len(manager.clients["private"]) != 1 || manager.clients["private"][0].Email != "selected" {
		t.Fatal("retry did not complete revocation")
	}
	state = app.store.Snapshot()
	user, _ = findUser(&state, "existing")
	if user.SubscriptionToken != "keep-token" {
		t.Fatal("subscription token changed")
	}
	if !serverView(state.Servers[1]).Private {
		t.Fatal("private flag missing from API view")
	}
}
