package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func TestFirstRunAndSessionPersistence(t *testing.T) {
	dir := t.TempDir()
	store, err := NewStore(filepath.Join(dir, "master.json"))
	if err != nil {
		t.Fatal(err)
	}
	app := NewApp(Config{DataDir: dir}, store)
	if err = app.initializeSettings(); err != nil {
		t.Fatal(err)
	}
	request := func(method, path, body string, cookie *http.Cookie) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		if cookie != nil {
			r.AddCookie(cookie)
		}
		w := httptest.NewRecorder()
		app.Handler().ServeHTTP(w, r)
		return w
	}
	if w := request("GET", "/api/overview", "", nil); w.Code != 401 {
		t.Fatal(w.Code)
	}
	payload := map[string]string{"base_url": "https://vpn.example.org", "certbot_email": "admin@example.org", "password": "a long admin password", "duckdns_token": "secret-duckdns-token", "setup_token": "wrong"}
	data, _ := json.Marshal(payload)
	if w := request("PUT", "/api/settings", string(data), nil); w.Code != 403 {
		t.Fatal(w.Code)
	}
	payload["setup_token"] = app.settings().SetupToken
	data, _ = json.Marshal(payload)
	w := request("PUT", "/api/settings", string(data), nil)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	cookie := w.Result().Cookies()[0]
	if !cookie.HttpOnly || !cookie.Secure || cookie.SameSite != http.SameSiteStrictMode {
		t.Fatal("unsafe cookie")
	}
	if app.needsSetup() || app.settings().SetupToken != "" {
		t.Fatal("setup not completed")
	}
	if strings.Contains(w.Body.String(), "secret-duckdns-token") || strings.Contains(w.Body.String(), "a long admin password") {
		t.Fatal("secret exposed")
	}
	if w = request("GET", "/api/overview", "", nil); w.Code != 401 {
		t.Fatal("unauthenticated overview")
	}
	if w = request("GET", "/api/overview", "", cookie); w.Code != 200 {
		t.Fatal(w.Code)
	}
	if w = request("PUT", "/api/settings", string(data), nil); w.Code != 401 {
		t.Fatal("setup token reused")
	}
	reopened, err := NewStore(filepath.Join(dir, "master.json"))
	if err != nil {
		t.Fatal(err)
	}
	app = NewApp(Config{DataDir: dir}, reopened)
	if w = request("GET", "/api/settings", "", cookie); w.Code != 200 {
		t.Fatal("session lost after restart")
	}
	if w = request("POST", "/api/login", `{"password":"a long admin password"}`, nil); w.Code != 200 {
		t.Fatal(w.Code)
	}
	r := httptest.NewRequest("PUT", "/api/settings", strings.NewReader(string(data)))
	r.AddCookie(cookie)
	r.Header.Set("Origin", "https://attacker.example.org")
	w = httptest.NewRecorder()
	app.Handler().ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatal("cross origin accepted")
	}
	payload["password"] = "a different admin password"
	data, _ = json.Marshal(payload)
	if w = request("PUT", "/api/settings", string(data), cookie); w.Code != 200 {
		t.Fatal(w.Code)
	}
	if w = request("GET", "/api/overview", "", cookie); w.Code != 401 {
		t.Fatal("old session survived password change")
	}
}
func TestLegacySettingsImportAndEnvironmentPriority(t *testing.T) {
	app := newTestApp(t, State{Version: stateVersion, Servers: []Server{{DuckDNSToken: "common"}, {DuckDNSToken: "common"}}, Settings: Settings{BaseURL: "https://stored.example.org"}})
	if err := app.initializeSettings(); err != nil {
		t.Fatal(err)
	}
	if s := app.settings(); s.DuckDNSToken != "common" || s.BaseURL != "https://master.example.org" || app.needsSetup() {
		t.Fatal("legacy import/priority")
	}
}
func TestEmptyEnvironmentAllowsWizard(t *testing.T) {
	for _, key := range []string{"BASE_URL", "CERTBOT_EMAIL", "PORT", "DATA_DIR", "LISTEN_ADDRESS"} {
		t.Setenv(key, "")
	}
	if _, err := loadConfigFrom(filepath.Join(t.TempDir(), "missing"), t.TempDir()); err != nil {
		t.Fatal(err)
	}
}
