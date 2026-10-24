package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newTestApp(t *testing.T, state State) *App {
	t.Helper()
	store, err := NewStore(filepath.Join(t.TempDir(), "master.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Update(func(target *State) error { *target = state; return nil }); err != nil {
		t.Fatal(err)
	}
	return NewApp(Config{BaseURL: "https://master.example.org", CertbotEmail: "admin@example.org"}, store)
}

func TestOverviewDoesNotExposeSecrets(t *testing.T) {
	app := newTestApp(t, State{Version: stateVersion, Servers: []Server{{
		ID: "server-1", Address: "192.0.2.1:22", DuckDNSURL: "one.duckdns.org",
		SSHPrivateKey: "PRIVATE", SSHPassphrase: "PASSPHRASE", DuckDNSToken: "TOKEN",
	}}, Users: []User{}})
	request := httptest.NewRequest(http.MethodGet, "/api/overview", nil)
	response := httptest.NewRecorder()
	app.Handler().ServeHTTP(response, request)
	body := response.Body.String()
	if response.Code != http.StatusOK {
		t.Fatalf("unexpected status %d", response.Code)
	}
	for _, secret := range []string{"PRIVATE", "PASSPHRASE", "TOKEN", "PASSWORD"} {
		if strings.Contains(body, secret) {
			t.Fatalf("secret %q leaked into overview", secret)
		}
	}
}

func TestMihomoSubscriptionAndProviders(t *testing.T) {
	readyURI := "vless://11111111-1111-1111-1111-111111111111@one.duckdns.org:443?encryption=none&flow=xtls-rprx-vision&security=reality&sni=one.duckdns.org&fp=chrome&pbk=public-key&sid=abcd&type=tcp#Profile"
	app := newTestApp(t, State{Version: stateVersion,
		Servers: []Server{{ID: "one", DisplayName: "Нидерланды", DuckDNSURL: "one.duckdns.org"}, {ID: "two", DuckDNSURL: "two.duckdns.org"}},
		Users: []User{{ID: "user", SubscriptionToken: "valid-token", Links: map[string]UserLink{
			"one": {ServerID: "one", Status: "ready", URI: readyURI},
			"two": {ServerID: "two", Status: "error", URI: "vless://hidden"},
		}, CreatedAt: time.Now()}},
	})
	response := httptest.NewRecorder()
	app.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/subscribe/valid-token", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("unexpected subscription: %d %q", response.Code, response.Body.String())
	}
	if contentType := response.Header().Get("Content-Type"); !strings.HasPrefix(contentType, "text/yaml") {
		t.Fatalf("unexpected content type %s", contentType)
	}
	if response.Header().Get("profile-update-interval") != "6" {
		t.Fatal("profile update interval header is missing")
	}
	for _, expected := range []string{
		"mode: rule",
		"https://master.example.org/subscribe/valid-token/proxies",
		"https://master.example.org/mihomo/direct-rules",
		"interval: 3600",
		`name: "Рекомендуемый"`,
		"type: fallback",
		"hidden: true",
		`default-selected: "Рекомендуемый"`,
		"RULE-SET,direct-rules,DIRECT",
	} {
		if !strings.Contains(response.Body.String(), expected) {
			t.Fatalf("Mihomo config does not contain %q", expected)
		}
	}

	provider := httptest.NewRecorder()
	app.Handler().ServeHTTP(provider, httptest.NewRequest(http.MethodGet, "/subscribe/valid-token/proxies", nil))
	if provider.Code != http.StatusOK || !strings.Contains(provider.Body.String(), `name: "Нидерланды"`) || !strings.Contains(provider.Body.String(), "type: vless") || !strings.Contains(provider.Body.String(), "public-key: \"public-key\"") {
		t.Fatalf("unexpected proxy provider: %d %q", provider.Code, provider.Body.String())
	}
	if strings.Contains(provider.Body.String(), "hidden") || strings.Contains(provider.Body.String(), "vless://") {
		t.Fatal("proxy provider contains an unavailable or unconverted link")
	}

	directRules := httptest.NewRecorder()
	app.Handler().ServeHTTP(directRules, httptest.NewRequest(http.MethodGet, "/mihomo/direct-rules", nil))
	if directRules.Code != http.StatusOK || !strings.Contains(directRules.Body.String(), `"DOMAIN-SUFFIX,ru"`) || !strings.Contains(directRules.Body.String(), `"DOMAIN-SUFFIX,2ip.io"`) || !strings.Contains(directRules.Body.String(), `"PROCESS-NAME,com.yandex.browser"`) {
		t.Fatalf("unexpected direct rules: %d %q", directRules.Code, directRules.Body.String())
	}
	if len(directAndroidPackages) != 463 {
		t.Fatalf("unexpected Android package count: %d", len(directAndroidPackages))
	}

	missing := httptest.NewRecorder()
	app.Handler().ServeHTTP(missing, httptest.NewRequest(http.MethodGet, "/subscribe/missing", nil))
	if missing.Code != http.StatusNotFound {
		t.Fatalf("unexpected missing status %d", missing.Code)
	}
	missingProvider := httptest.NewRecorder()
	app.Handler().ServeHTTP(missingProvider, httptest.NewRequest(http.MethodGet, "/subscribe/missing/proxies", nil))
	if missingProvider.Code != http.StatusNotFound {
		t.Fatalf("unexpected missing provider status %d", missingProvider.Code)
	}
}

func TestUpdateServerDisplayName(t *testing.T) {
	app := newTestApp(t, State{Version: stateVersion, Servers: []Server{{ID: "server", DuckDNSURL: "one.duckdns.org"}}, Users: []User{}})
	response := httptest.NewRecorder()
	app.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodPatch, "/api/servers/server", strings.NewReader(`{"display_name":"  Финляндия  "}`)))
	if response.Code != http.StatusOK {
		t.Fatalf("rename returned %d: %s", response.Code, response.Body.String())
	}
	if got := app.store.Snapshot().Servers[0].DisplayName; got != "Финляндия" {
		t.Fatalf("unexpected stored display name %q", got)
	}
	if strings.Contains(response.Body.String(), "ssh_private_key") || !strings.Contains(response.Body.String(), `"display_name":"Финляндия"`) {
		t.Fatalf("unexpected rename response: %s", response.Body.String())
	}

	reset := httptest.NewRecorder()
	app.Handler().ServeHTTP(reset, httptest.NewRequest(http.MethodPatch, "/api/servers/server", strings.NewReader(`{"display_name":""}`)))
	if reset.Code != http.StatusOK || app.store.Snapshot().Servers[0].DisplayName != "" {
		t.Fatal("display name was not reset")
	}

	missing := httptest.NewRecorder()
	app.Handler().ServeHTTP(missing, httptest.NewRequest(http.MethodPatch, "/api/servers/missing", strings.NewReader(`{"display_name":"Name"}`)))
	if missing.Code != http.StatusNotFound {
		t.Fatalf("unexpected missing rename status %d", missing.Code)
	}
}

func TestEmbeddedUIAndClipboardFallback(t *testing.T) {
	app := newTestApp(t, State{Version: stateVersion, Servers: []Server{}, Users: []User{}})
	index := httptest.NewRecorder()
	app.Handler().ServeHTTP(index, httptest.NewRequest(http.MethodGet, "/", nil))
	if index.Code != http.StatusOK || !strings.Contains(index.Body.String(), "Серверы") || !strings.Contains(index.Body.String(), "Пользователи") {
		t.Fatalf("unexpected embedded index: %d", index.Code)
	}
	for _, removed := range []string{`class="topbar"`, `class="hero"`, "Синхронизировать всё"} {
		if strings.Contains(index.Body.String(), removed) {
			t.Fatalf("removed header content %q is still embedded", removed)
		}
	}
	if !strings.Contains(index.Body.String(), "Синхронизировать пользователей") {
		t.Fatal("user synchronization action is missing from embedded UI")
	}
	for _, action := range []string{"Закрыть", "Продолжить установку", "Код первоначальной настройки"} {
		if !strings.Contains(index.Body.String(), action) {
			t.Fatalf("failed provisioning action %q is missing", action)
		}
	}
	javascript := httptest.NewRecorder()
	app.Handler().ServeHTTP(javascript, httptest.NewRequest(http.MethodGet, "/assets/app.js", nil))
	if javascript.Code != http.StatusOK || !strings.Contains(javascript.Body.String(), "navigator.clipboard") || !strings.Contains(javascript.Body.String(), "copy-fallback") {
		t.Fatalf("clipboard fallback is missing from embedded UI")
	}
	if !strings.Contains(javascript.Body.String(), "/resume") || !strings.Contains(javascript.Body.String(), "data-resume-server") {
		t.Fatal("failed provisioning cleanup or retry behavior is missing")
	}
	if !strings.Contains(javascript.Body.String(), "data-edit-server") || !strings.Contains(javascript.Body.String(), "display_name") {
		t.Fatal("server display name editor is missing")
	}
	if !strings.Contains(javascript.Body.String(), "Ubuntu 22.04, 24.04 или 26.04") {
		t.Fatal("supported Ubuntu LTS versions are missing from embedded UI")
	}
	stylesheet := httptest.NewRecorder()
	app.Handler().ServeHTTP(stylesheet, httptest.NewRequest(http.MethodGet, "/assets/app.css", nil))
	if stylesheet.Code != http.StatusOK || !strings.Contains(stylesheet.Body.String(), "overflow-x: hidden") {
		t.Fatal("dialog horizontal overflow protection is missing")
	}
}

func TestCleanupServerHTTPStartsJobWithoutReturningSecrets(t *testing.T) {
	privateKey, _ := testPrivateKey(t)
	app := newTestApp(t, State{Version: stateVersion, Servers: []Server{}, Users: []User{}})
	body, err := json.Marshal(AddServerRequest{
		Address: "127.0.0.1:1", PrivateKey: privateKey,
		DuckDNSURL: "cleanup.duckdns.org", DuckDNSToken: "duck-secret",
	})
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	app.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/servers/cleanup", strings.NewReader(string(body))))
	if response.Code != http.StatusAccepted {
		t.Fatalf("cleanup returned %d: %s", response.Code, response.Body.String())
	}
	if strings.Contains(response.Body.String(), "duck-secret") || strings.Contains(response.Body.String(), "PRIVATE KEY") {
		t.Fatal("cleanup response exposed request secrets")
	}
	var result map[string]string
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	job, exists := app.jobs.Get(result["job_id"])
	if !exists {
		t.Fatal("cleanup job is missing")
	}
	waitForJob(t, job)
	if job.Snapshot().Kind != "server_cleanup" {
		t.Fatalf("unexpected job kind: %s", job.Snapshot().Kind)
	}
}

func TestUserHTTPCreateAndDelete(t *testing.T) {
	app := newTestApp(t, State{Version: stateVersion, Servers: []Server{}, Users: []User{}})
	create := httptest.NewRecorder()
	app.Handler().ServeHTTP(create, httptest.NewRequest(http.MethodPost, "/api/users", strings.NewReader(`{"email":"Обычное имя профиля"}`)))
	if create.Code != http.StatusAccepted {
		t.Fatalf("create returned %d: %s", create.Code, create.Body.String())
	}
	var createResult map[string]string
	if err := json.Unmarshal(create.Body.Bytes(), &createResult); err != nil {
		t.Fatal(err)
	}
	createJob, exists := app.jobs.Get(createResult["job_id"])
	if !exists {
		t.Fatal("create job is missing")
	}
	waitForJob(t, createJob)
	users := app.store.Snapshot().Users
	if len(users) != 1 || users[0].Email != "Обычное имя профиля" {
		t.Fatalf("user was not created: %+v", users)
	}

	deleteResponse := httptest.NewRecorder()
	app.Handler().ServeHTTP(deleteResponse, httptest.NewRequest(http.MethodDelete, "/api/users/"+users[0].ID, strings.NewReader(`{}`)))
	if deleteResponse.Code != http.StatusAccepted {
		t.Fatalf("delete returned %d: %s", deleteResponse.Code, deleteResponse.Body.String())
	}
	var deleteResult map[string]string
	if err := json.Unmarshal(deleteResponse.Body.Bytes(), &deleteResult); err != nil {
		t.Fatal(err)
	}
	deleteJob, exists := app.jobs.Get(deleteResult["job_id"])
	if !exists {
		t.Fatal("delete job is missing")
	}
	waitForJob(t, deleteJob)
	if len(app.store.Snapshot().Users) != 0 {
		t.Fatal("user was not deleted")
	}
}

func TestServerHTTPRejectsUnknownFields(t *testing.T) {
	app := newTestApp(t, State{Version: stateVersion, Servers: []Server{}, Users: []User{}})
	response := httptest.NewRecorder()
	app.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/servers", strings.NewReader(`{"unexpected":true}`)))
	if response.Code != http.StatusBadRequest {
		t.Fatalf("unexpected status %d", response.Code)
	}
}

func TestUpdateServerPrivacyReconcilesAccess(t *testing.T) {
	app := newTestApp(t, State{Version: stateVersion,
		Servers: []Server{{ID: "server", DuckDNSURL: "one.duckdns.org"}},
		Users: []User{
			{ID: "all", Email: "all", ServerMode: "all"},
			{ID: "selected", Email: "selected", ServerMode: "selected", ServerIDs: []string{"server"}},
		},
	})
	manager := &fakeManager{}
	app.servers = manager
	patch := func(body string, status int) {
		t.Helper()
		response := httptest.NewRecorder()
		app.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodPatch, "/api/servers/server", strings.NewReader(body)))
		if response.Code != status {
			t.Fatalf("PATCH returned %d: %s", response.Code, response.Body.String())
		}
		if status == http.StatusAccepted {
			var result map[string]string
			if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			job, ok := app.jobs.Get(result["job_id"])
			if !ok {
				t.Fatal("missing reconciliation job")
			}
			waitForJob(t, job)
			if job.Snapshot().Status == "error" {
				t.Fatal(job.Snapshot())
			}
		}
	}
	patch(`{"display_name":"Private","private":true}`, http.StatusAccepted)
	if clients := manager.clients["server"]; len(clients) != 1 || clients[0].Email != "selected" {
		t.Fatalf("unexpected private clients: %+v", clients)
	}
	patch(`{"display_name":"Renamed"}`, http.StatusOK)
	if !app.store.Snapshot().Servers[0].Private || manager.calls["server"] != 1 {
		t.Fatal("rename changed privacy or triggered reconciliation")
	}
	patch(`{"display_name":"Public","private":false}`, http.StatusAccepted)
	if len(manager.clients["server"]) != 2 || app.store.Snapshot().Servers[0].Private {
		t.Fatal("public access was not restored")
	}
	manager.offline = true
	patch(`{"display_name":"Private","private":true}`, http.StatusAccepted)
	state := app.store.Snapshot()
	if state.Users[0].Links["server"].Status != "revoking" {
		t.Fatal("offline revocation was not tracked")
	}
	if got := string(buildMihomoProvider(state, &state.Users[0])); got != "proxies: []\n" {
		t.Fatalf("private server leaked into all-user subscription: %s", got)
	}
	manager.offline = false
	patch(`{"display_name":"Public","private":false}`, http.StatusAccepted)
	if len(manager.clients["server"]) != 2 {
		t.Fatal("access not restored after offline change")
	}
}
