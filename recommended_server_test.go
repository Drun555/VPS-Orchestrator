package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRecommendedProviderOrderAndPermissions(t *testing.T) {
	state := State{Version: stateVersion, Servers: []Server{
		{ID: "nl", DisplayName: "Нидерланды"},
		{ID: "fi", DisplayName: "Финляндия"},
		{ID: "de", DisplayName: "Германия"},
	}}
	user := User{ID: "u", ServerMode: "all", Links: map[string]UserLink{}}
	for _, server := range state.Servers {
		user.Links[server.ID] = UserLink{Status: "ready", URI: "vless://uuid@" + server.ID + ".example.org:443?security=reality&pbk=key"}
	}
	check := func(want ...string) {
		t.Helper()
		body := string(buildMihomoProvider(state, &user))
		var got []string
		for _, line := range strings.Split(body, "\n") {
			if strings.HasPrefix(line, "    server: ") {
				got = append(got, strings.TrimPrefix(line, "    server: "))
			}
		}
		for i := range want {
			want[i] = yamlQuote(want[i] + ".example.org")
		}
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
	check("nl", "fi", "de")
	user.RecommendedServerID = "de"
	check("de", "nl", "fi")
	state.Servers[2].Private = true
	check("nl", "fi")
	user.ServerMode = "selected"
	user.ServerIDs = []string{"de", "fi"}
	check("de", "fi")
	user.Links["de"] = UserLink{Status: "error"}
	check("fi")
	user.Links["de"] = UserLink{Status: "ready", URI: "invalid"}
	check("fi")
	user.RecommendedServerID = "deleted"
	check("fi")
	user.ServerIDs = nil
	if got := string(buildMihomoProvider(state, &user)); got != "proxies: []\n" {
		t.Fatal(got)
	}
}

func TestPerClientRecommendationPersistenceAndIsolation(t *testing.T) {
	app := newTestApp(t, State{Version: stateVersion, Servers: []Server{{ID: "nl", DisplayName: "NL", DuckDNSURL: "nl.example.org"}, {ID: "fi", DisplayName: "FI", DuckDNSURL: "fi.example.org"}, {ID: "private", Private: true}}})
	app.servers = &fakeManager{}
	run := func(work func(*JobReporter) error, wantError bool) {
		t.Helper()
		job := runTestJob(t, app, work)
		if (job.Status == "error") != wantError {
			t.Fatal(job)
		}
	}
	for _, name := range []string{"first", "second"} {
		run(func(r *JobReporter) error {
			return app.createUser(context.Background(), AddUserRequest{Email: name, ServerMode: "all"}, r)
		}, false)
	}
	first, second := app.store.Snapshot().Users[0].ID, app.store.Snapshot().Users[1].ID
	update := func(id, mode string, ids []string, recommended *string, wantError bool) {
		t.Helper()
		run(func(r *JobReporter) error {
			return app.updateUser(context.Background(), id, UpdateUserRequest{ServerMode: mode, ServerIDs: ids, RecommendedServerID: recommended}, r)
		}, wantError)
	}
	fi, nl, private, missing, empty := "fi", "nl", "private", "missing", ""
	update(first, "all", nil, &fi, false)
	update(second, "all", nil, &nl, false)
	reopened, err := NewStore(app.store.path)
	if err != nil {
		t.Fatal(err)
	}
	app.store = reopened
	state := app.store.Snapshot()
	if state.Users[0].RecommendedServerID != "fi" || state.Users[1].RecommendedServerID != "nl" {
		t.Fatal("per-client selection lost")
	}
	firstProvider := string(buildMihomoProvider(state, &state.Users[0]))
	secondProvider := string(buildMihomoProvider(state, &state.Users[1]))
	if !strings.Contains(firstProvider, `name: "FI"`) || !strings.Contains(firstProvider, `name: "NL"`) || !strings.Contains(secondProvider, `name: "FI"`) || !strings.Contains(secondProvider, `name: "NL"`) || strings.Index(firstProvider, `name: "FI"`) > strings.Index(firstProvider, `name: "NL"`) || strings.Index(secondProvider, `name: "NL"`) > strings.Index(secondProvider, `name: "FI"`) {
		t.Fatal("clients share provider priority")
	}
	// Use the exposed per-user values to verify API round-trip.
	w := httptest.NewRecorder()
	app.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/overview", nil))
	var overview struct {
		Users []UserView `json:"users"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &overview); err != nil {
		t.Fatal(err)
	}
	if overview.Users[0].RecommendedServerID != "fi" || overview.Users[1].RecommendedServerID != "nl" {
		t.Fatal("API lost client recommendations")
	}
	update(first, "all", nil, nil, false)
	if app.store.Snapshot().Users[0].RecommendedServerID != "fi" {
		t.Fatal("omitted selection was reset")
	}
	update(first, "all", nil, &private, true)
	update(first, "all", nil, &missing, true)
	if app.store.Snapshot().Users[0].RecommendedServerID != "fi" {
		t.Fatal("invalid update mutated selection")
	}
	update(first, "selected", []string{"nl"}, nil, false)
	if app.store.Snapshot().Users[0].RecommendedServerID != "" {
		t.Fatal("removed access retained recommendation")
	}
	update(first, "selected", []string{"nl"}, &fi, true)
	update(first, "selected", []string{"private"}, &private, false)
	update(first, "all", nil, &fi, false)
	update(first, "all", nil, &empty, false)
	if app.store.Snapshot().Users[0].RecommendedServerID != "" {
		t.Fatal("default was not restored")
	}
	update(first, "all", nil, &fi, false)
	if err := app.forgetServer("fi"); err != nil {
		t.Fatal(err)
	}
	state = app.store.Snapshot()
	if state.Users[0].RecommendedServerID != "" || state.Users[1].RecommendedServerID != "nl" {
		t.Fatal("delete affected wrong recommendation")
	}
	run(func(r *JobReporter) error {
		return app.createUser(context.Background(), AddUserRequest{Email: "bad", ServerMode: "all", RecommendedServerID: "private"}, r)
	}, true)
}
