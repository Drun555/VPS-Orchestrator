package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type fakeSiteManager struct {
	fakeManager
	fail    bool
	updates int
}

func (m *fakeSiteManager) UpdateSite(_ context.Context, before, after Server, email string, commit func() error, r *JobReporter) error {
	m.updates++
	if m.fail {
		return errors.New("SSH unavailable")
	}
	return commit()
}
func patchServer(t *testing.T, app *App, id, body string) *httptest.ResponseRecorder {
	t.Helper()
	response := httptest.NewRecorder()
	app.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodPatch, "/api/servers/"+id, strings.NewReader(body)))
	return response
}
func TestServerSiteHTTPValidation(t *testing.T) {
	app := newTestApp(t, State{Version: stateVersion, Servers: []Server{
		{ID: "one", DuckDNSURL: "one.duckdns.org", DisplayName: "Keep name"},
		{ID: "two", DuckDNSURL: "two.duckdns.org"},
		{ID: "setup", DuckDNSURL: "setup.duckdns.org", Provisioning: true},
	}})
	for _, tc := range []struct {
		id, body string
		code     int
	}{
		{"one", `{"duckdns_url":"https://bad.duckdns.org"}`, 400},
		{"one", `{"duckdns_url":""}`, 400},
		{"one", `{"site_template":"../index"}`, 400},
		{"one", `{"duckdns_url":"TWO.DUCKDNS.ORG."}`, 409},
		{"setup", `{"site_template":"putin-video"}`, 409},
		{"setup", `{"duckdns_url":"new.duckdns.org"}`, 409},
		{"missing", `{"site_template":"putin-video"}`, 404},
	} {
		if response := patchServer(t, app, tc.id, tc.body); response.Code != tc.code {
			t.Fatalf("%s %s: %d %s", tc.id, tc.body, response.Code, response.Body.String())
		}
	}
	if app.store.Snapshot().Servers[0].DisplayName != "Keep name" {
		t.Fatal("invalid patch changed name")
	}
}
func TestServerSiteUpdateRefreshesSubscriptionsWithoutRotatingIdentity(t *testing.T) {
	server := Server{ID: "one", DisplayName: "Keep name", DuckDNSURL: "one.duckdns.org", SiteTemplate: "file-hosting", SSHPrivateKey: "PRIVATE", DuckDNSToken: "SECRET"}
	user := User{ID: "user", Email: "test", ServerMode: "all", SubscriptionToken: "keep-token", Links: map[string]UserLink{
		"one": {ServerID: "one", ClientID: "keep-uuid", Status: "ready", URI: clientURI(server, "keep-uuid", "test", "keep-public", "abcd")},
	}}
	app := newTestApp(t, State{Version: stateVersion, Servers: []Server{server}, Users: []User{user}})
	manager := &fakeSiteManager{}
	app.servers = manager
	response := patchServer(t, app, "one", `{"duckdns_url":"NEW.DUCKDNS.ORG.","site_template":"putin-video"}`)
	if response.Code != 202 {
		t.Fatal(response.Code, response.Body.String())
	}
	var result map[string]string
	json.Unmarshal(response.Body.Bytes(), &result)
	job, ok := app.jobs.Get(result["job_id"])
	if !ok {
		t.Fatal("missing job")
	}
	waitForJob(t, job)
	if job.Snapshot().Status != "success" {
		t.Fatal(job.Snapshot())
	}
	state := app.store.Snapshot()
	s := state.Servers[0]
	u := state.Users[0]
	if s.DisplayName != server.DisplayName || s.DuckDNSURL != "new.duckdns.org" || s.SiteTemplate != "putin-video" || s.SSHPrivateKey != "PRIVATE" {
		t.Fatal("wrong server settings")
	}
	if u.SubscriptionToken != "keep-token" || u.Links["one"].ClientID != "keep-uuid" {
		t.Fatal("identity rotated")
	}
	provider := string(buildMihomoProvider(state, &u))
	for _, expected := range []string{"new.duckdns.org", "keep-public", "keep-uuid", "abcd"} {
		if !strings.Contains(provider, expected) {
			t.Fatalf("provider missing %s", expected)
		}
	}
	if strings.Contains(provider, "one.duckdns.org") {
		t.Fatal("provider retained old domain")
	}
	repeated := patchServer(t, app, "one", `{"duckdns_url":"new.duckdns.org","site_template":"putin-video"}`)
	if repeated.Code != 200 || manager.updates != 1 {
		t.Fatal("unchanged settings scheduled another SSH operation")
	}
	if strings.Contains(response.Body.String(), "PRIVATE") || strings.Contains(response.Body.String(), "SECRET") {
		t.Fatal("secrets leaked")
	}
	reopened, err := NewStore(app.store.path)
	if err != nil {
		t.Fatal(err)
	}
	if reopened.Snapshot().Servers[0].DuckDNSURL != "new.duckdns.org" {
		t.Fatal("change not persisted")
	}
}
func TestFailedServerSiteUpdateDoesNotSaveRequestedSettings(t *testing.T) {
	app := newTestApp(t, State{Version: stateVersion, Servers: []Server{{ID: "one", DisplayName: "Old", DuckDNSURL: "one.duckdns.org", SiteTemplate: "file-hosting"}}})
	app.servers = &fakeSiteManager{fail: true}
	job := runTestJob(t, app, func(r *JobReporter) error {
		domain, template, name := "new.duckdns.org", "pioneer-archive", "New"
		return app.updateServerSite(context.Background(), "one", UpdateServerRequest{DuckDNSURL: &domain, SiteTemplate: &template, DisplayName: &name}, r)
	})
	if job.Status != "error" {
		t.Fatal(job)
	}
	server := app.store.Snapshot().Servers[0]
	if server.DisplayName != "Old" || server.DuckDNSURL != "one.duckdns.org" || server.SiteTemplate != "file-hosting" {
		t.Fatal("failed operation overwrote settings")
	}
}

func TestSiteAndPrivacyCanChangeTogether(t *testing.T) {
	app := newTestApp(t, State{Version: stateVersion, Servers: []Server{{ID: "one", DuckDNSURL: "one.duckdns.org"}}, Users: []User{
		{ID: "all", Email: "all", ServerMode: "all", Links: map[string]UserLink{"one": {ServerID: "one", ClientID: "old", Status: "ready"}}},
		{ID: "selected", Email: "selected", ServerMode: "selected", ServerIDs: []string{"one"}},
	}})
	manager := &fakeSiteManager{}
	app.servers = manager
	domain, template, private := "new.duckdns.org", "putin-video", true
	job := runTestJob(t, app, func(r *JobReporter) error {
		return app.updateServerSite(context.Background(), "one", UpdateServerRequest{DuckDNSURL: &domain, SiteTemplate: &template, Private: &private}, r)
	})
	if job.Status != "success" {
		t.Fatal(job)
	}
	state := app.store.Snapshot()
	if !state.Servers[0].Private || len(manager.clients["one"]) != 1 || manager.clients["one"][0].Email != "selected" {
		t.Fatal("private access was not reconciled")
	}
	if _, exists := state.Users[0].Links["one"]; exists {
		t.Fatal("all user retained access")
	}
	if !strings.Contains(state.Users[1].Links["one"].URI, "new.duckdns.org") {
		t.Fatal("explicit assignment has old domain")
	}
}
