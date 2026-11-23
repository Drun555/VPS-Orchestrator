package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func balanceFixture(n int) State {
	state := State{Version: stateVersion, Servers: []Server{{ID: "nl", DisplayName: "Нидерланды", Status: "ready"}, {ID: "fi", DisplayName: "Финляндия", Status: "ready"}}}
	for i := 0; i < n; i++ {
		u := User{ID: fmt.Sprintf("u%02d", i), Email: fmt.Sprintf("Пользователь %02d", i), ServerMode: "all", SubscriptionToken: fmt.Sprintf("secret-token-%d", i), Links: map[string]UserLink{}}
		for _, s := range state.Servers {
			u.Links[s.ID] = UserLink{Status: "ready", ClientID: u.ID, URI: "vless://" + u.ID + "@" + s.ID + ".example.org:443?security=reality&pbk=public"}
		}
		state.Users = append(state.Users, u)
	}
	return state
}
func balanceNow() time.Time { return time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC) }
func emptyBalanceMetrics() balanceMetrics {
	return balanceMetrics{Month: map[string]float64{}, Recent: map[string]float64{}, Live: map[string]trafficLive{}}
}
func planUserTargets(state State, v balanceView) map[string]string {
	out := map[string]string{}
	for _, u := range state.Users {
		out[u.ID] = balanceCurrentServer(state, u)
	}
	for _, move := range v.Moves {
		out[move.UserID] = move.To
	}
	return out
}
func TestBalanceWeightedDistributionStableAndNoMutation(t *testing.T) {
	state := balanceFixture(12)
	state.Servers[1].BalanceWeight = 2
	before := cloneState(state)
	plan := buildBalance(state, emptyBalanceMetrics(), balanceNow(), true)
	if !reflect.DeepEqual(state, before) {
		t.Fatal("preview mutated state")
	}
	if plan.Servers[0].UsersAfter != 4 || plan.Servers[1].UsersAfter != 8 {
		t.Fatalf("expected 4:8 distribution: %+v", plan.Servers)
	}
	if len(plan.Moves) != 8 {
		t.Fatal(plan.Moves)
	}
	for _, move := range plan.Moves {
		u, _ := findUser(&state, move.UserID)
		u.RecommendedServerID = move.To
		pin := false
		u.RecommendationPinned = &pin
	}
	if second := buildBalance(state, emptyBalanceMetrics(), balanceNow(), true); len(second.Moves) != 0 {
		t.Fatalf("unchanged input reshuffles users: %+v", second.Moves)
	}
}
func TestBalanceUsesConsumptionAndCountsPinnedLoad(t *testing.T) {
	state := balanceFixture(5)
	m := emptyBalanceMetrics()
	now := balanceNow()
	m.StartedAt = now.Add(-10 * 24 * time.Hour)
	m.Recent["u00"] = 100e9
	for _, u := range state.Users[1:] {
		m.Recent[u.ID] = 10e9
	}
	plan := buildBalance(state, m, now, true)
	if plan.Servers[0].DailyAfter != 4e9 || plan.Servers[1].DailyAfter != 10e9 || len(plan.Moves) != 1 || plan.Moves[0].UserID != "u00" {
		t.Fatalf("demand ignored: %+v", plan.Servers)
	}
	state.Users[0].RecommendedServerID = "fi" // Legacy manual recommendation is pinned.
	plan = buildBalance(state, m, now, true)
	if plan.Servers[1].PinnedUsers != 1 || plan.Servers[1].DailyAfter != 10e9 || plan.Servers[0].UsersAfter != 4 {
		t.Fatalf("pinned consumption ignored: %+v", plan.Servers)
	}
	for _, move := range plan.Moves {
		if move.UserID == "u00" {
			t.Fatal("moved manually pinned user")
		}
	}
}
func TestBalancePermissionsAndUnavailableServers(t *testing.T) {
	for _, tc := range []struct {
		name   string
		modify func(*State, *balanceMetrics)
	}{
		{"private", func(s *State, m *balanceMetrics) { s.Servers[1].Private = true }},
		{"disabled", func(s *State, m *balanceMetrics) { s.Servers[1].BalanceDisabled = true }},
		{"provisioning", func(s *State, m *balanceMetrics) { s.Servers[1].Provisioning = true }},
		{"error", func(s *State, m *balanceMetrics) { s.Servers[1].Status = "error" }},
		{"offline", func(s *State, m *balanceMetrics) { m.Live["fi"] = trafficLive{Status: "offline"} }},
		{"revoking", func(s *State, m *balanceMetrics) {
			for i := range s.Users {
				s.Users[i].Links["fi"] = UserLink{Status: "revoking"}
			}
		}},
		{"bad URI", func(s *State, m *balanceMetrics) {
			for i := range s.Users {
				s.Users[i].Links["fi"] = UserLink{Status: "ready", URI: "invalid"}
			}
		}},
		{"monthly exhausted", func(s *State, m *balanceMetrics) { s.Servers[1].MonthlyLimitGB = 10; m.Month["fi"] = 10e9 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state := balanceFixture(4)
			m := emptyBalanceMetrics()
			tc.modify(&state, &m)
			p := buildBalance(state, m, balanceNow(), true)
			if len(p.Moves) != 0 {
				t.Fatalf("moved to unavailable server: %+v", p.Moves)
			}
		})
	}
	state := balanceFixture(4)
	state.Servers[1].Private = true
	state.Users[0].ServerMode = "selected"
	state.Users[0].ServerIDs = []string{"fi"}
	p := buildBalance(state, emptyBalanceMetrics(), balanceNow(), true)
	targets := planUserTargets(state, p)
	if targets["u00"] != "fi" || p.Servers[1].UsersAfter != 1 {
		t.Fatalf("explicit private assignment not respected: %+v", targets)
	}
	state.Users[1].ServerMode = "selected"
	state.Users[1].ServerIDs = []string{}
	if planUserTargets(state, buildBalance(state, emptyBalanceMetrics(), balanceNow(), true))["u01"] != "" {
		t.Fatal("granted access to user with no assigned servers")
	}
}
func TestBalanceMonthlyForecastAndUnknownDemand(t *testing.T) {
	state := balanceFixture(4)
	state.Servers[1].MonthlyLimitGB = 10
	m := emptyBalanceMetrics()
	m.Month["fi"] = 9e9
	p := buildBalance(state, m, balanceNow(), true)
	if len(p.Moves) != 0 {
		t.Fatal("assigned traffic without monthly headroom")
	}
	state.Servers[1].MonthlyLimitGB = 20
	p = buildBalance(state, m, balanceNow(), true)
	if p.Servers[1].UsersAfter != 1 {
		t.Fatalf("forecast did not reserve headroom: %+v", p.Servers[1])
	}
	if p.BaselineDailyBytes != 1e9 || !p.Moves[0].Estimated || len(p.Warnings) == 0 {
		t.Fatal("unknown demand presented as measured")
	}
}
func TestBalanceTrafficWindowAndStaleRates(t *testing.T) {
	app := newTestApp(t, balanceFixture(1))
	now := balanceNow()
	tr, err := NewTraffic(filepath.Join(t.TempDir(), "traffic.json"), nil)
	if err != nil {
		t.Fatal(err)
	}
	app.traffic = tr
	tr.disk.StartedAt = now.Add(-60 * 24 * time.Hour)
	tr.disk.Buckets = map[string]trafficBucket{
		"current":  {Day: now.Truncate(24 * time.Hour), ServerID: "nl", UserID: "u00", TrafficBytes: TrafficBytes{Upload: 2, Download: 3}},
		"previous": {Day: time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC), ServerID: "deleted", UserID: "u00", TrafficBytes: TrafficBytes{Download: 7}},
		"old":      {Day: now.Add(-40 * 24 * time.Hour), ServerID: "nl", UserID: "u00", TrafficBytes: TrafficBytes{Download: 100}},
	}
	tr.live["nl"] = trafficLive{Status: "ready", At: now.Add(-16 * time.Second), Rates: map[string]trafficRate{"u00": {Download: 900}}}
	m := app.balanceMetrics(now)
	if m.Month["nl"] != 5 || m.Recent["u00"] != 12 {
		t.Fatalf("wrong traffic windows: %+v", m)
	}
	if m.Live["nl"].Status != "stale" || buildBalance(app.store.Snapshot(), m, now, false).Servers[0].Rate != nil {
		t.Fatal("stale rate shown as live")
	}
	tr.live["nl"] = trafficLive{Status: "ready", At: now, Rates: map[string]trafficRate{"u00": {Upload: 2, Download: 8}}}
	p := buildBalance(app.store.Snapshot(), app.balanceMetrics(now), now, false)
	if p.Servers[0].Rate.Download != 8 || p.Servers[0].Rate.Upload != 2 {
		t.Fatal("missing fresh rates")
	}
}
func balanceHTTP(t *testing.T, app *App, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	app.Handler().ServeHTTP(w, httptest.NewRequest(method, path, strings.NewReader(body)))
	return w
}
func balancePreview(t *testing.T, app *App) balanceView {
	t.Helper()
	w := balanceHTTP(t, app, http.MethodPost, "/api/balance/preview", "{}")
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	var v balanceView
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
		t.Fatal(err)
	}
	return v
}
func balanceApply(t *testing.T, app *App, id string) *httptest.ResponseRecorder {
	return balanceHTTP(t, app, http.MethodPost, "/api/balance/apply", `{"plan_id":"`+id+`"}`)
}
func TestBalancePreviewApplyAtomicPersistenceAndProviderOrder(t *testing.T) {
	initial := balanceFixture(6)
	app := newTestApp(t, initial)
	plan := balancePreview(t, app)
	if len(plan.Moves) != 3 || plan.ID == "" {
		t.Fatal(plan)
	}
	if !reflect.DeepEqual(app.store.Snapshot(), cloneState(initial)) {
		t.Fatal("preview changed stored data")
	}
	w := balanceApply(t, app, plan.ID)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	state := app.store.Snapshot()
	for _, move := range plan.Moves {
		u, _ := findUser(&state, move.UserID)
		before, _ := findUser(&initial, u.ID)
		if u.RecommendedServerID != move.To || recommendationPinned(*u) || !reflect.DeepEqual(u.Links, before.Links) || u.SubscriptionToken != before.SubscriptionToken {
			t.Fatal("wrong changes during apply")
		}
		provider := string(buildMihomoProvider(state, u))
		if strings.Index(provider, `server: "fi.example.org"`) > strings.Index(provider, `server: "nl.example.org"`) {
			t.Fatal("recommendation not applied to provider")
		}
	}
	reopened, err := NewStore(app.store.path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(reopened.Snapshot(), state) {
		t.Fatal("recommendations did not persist")
	}
	if balanceApply(t, app, plan.ID).Code != 409 {
		t.Fatal("replayed plan was applied")
	}
	if p := balancePreview(t, app); len(p.Moves) != 0 {
		t.Fatalf("repeat requires changes: %+v", p.Moves)
	}
	for _, path := range []string{"/api/balance", "/api/balance/preview"} {
		method := http.MethodGet
		body := ""
		if strings.HasSuffix(path, "preview") {
			method = http.MethodPost
			body = "{}"
		}
		response := balanceHTTP(t, app, method, path, body)
		if strings.Contains(response.Body.String(), "secret-token") || strings.Contains(response.Body.String(), "vless://") {
			t.Fatal("secrets leaked into balancing API")
		}
	}
}
func TestBalanceRejectsStaleExpiredAndUnhealthyPlans(t *testing.T) {
	for _, scenario := range []string{"assignment changed", "weight changed", "expired", "offline", "limit reached", "forecast changed", "restarted"} {
		t.Run(scenario, func(t *testing.T) {
			app := newTestApp(t, balanceFixture(6))
			plan := balancePreview(t, app)
			switch scenario {
			case "assignment changed":
				app.store.Update(func(s *State) error { s.Users[0].RecommendedServerID = "fi"; return nil })
			case "weight changed":
				app.store.Update(func(s *State) error { s.Servers[1].BalanceWeight = 2; return nil })
			case "expired":
				p := app.balancePlans[plan.ID]
				p.View.ExpiresAt = time.Now().Add(-time.Second)
				app.balancePlans[plan.ID] = p
			case "offline", "limit reached", "forecast changed":
				app.traffic, _ = NewTraffic(filepath.Join(t.TempDir(), "traffic.json"), nil)
				if scenario == "offline" {
					app.traffic.live["fi"] = trafficLive{Status: "offline"}
				} else {
					// The limit must be present when the plan is calculated.
					app.store.Update(func(s *State) error { s.Servers[1].MonthlyLimitGB = 1000; return nil })
					plan = balancePreview(t, app)
					bytes := int64(1000e9)
					if scenario == "forecast changed" {
						bytes = int64(999e9)
					}
					app.traffic.disk.Buckets["new"] = trafficBucket{Day: time.Now().UTC().Truncate(24 * time.Hour), ServerID: "fi", TrafficBytes: TrafficBytes{Download: bytes}}
				}
			case "restarted":
				app = NewApp(app.config, app.store)
			}
			before := app.store.Snapshot()
			w := balanceApply(t, app, plan.ID)
			if w.Code != 409 {
				t.Fatal(w.Code, w.Body.String())
			}
			if !reflect.DeepEqual(before, app.store.Snapshot()) {
				t.Fatal("stale plan partially mutated state")
			}
		})
	}
}
func TestBalanceSettingsAndManualPinRoundTrip(t *testing.T) {
	app := newTestApp(t, balanceFixture(2))
	for _, body := range []string{`{"balance_weight":0}`, `{"balance_weight":101}`, `{"monthly_limit_gb":-1}`, `{"monthly_limit_gb":10000001}`} {
		if w := patchServer(t, app, "nl", body); w.Code != 400 {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	w := patchServer(t, app, "nl", `{"balance_weight":3,"monthly_limit_gb":2000,"balance_disabled":true}`)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	s := app.store.Snapshot().Servers[0]
	if s.BalanceWeight != 3 || s.MonthlyLimitGB != 2000 || !s.BalanceDisabled {
		t.Fatal(s)
	}
	// Updating only metadata must not contact a live SSH manager.
	fi := "fi"
	no := false
	yes := true
	for _, tc := range []struct {
		pin  *bool
		want bool
	}{{nil, true}, {&no, false}, {&yes, true}} {
		job := runTestJob(t, app, func(r *JobReporter) error {
			return app.updateUser(context.Background(), "u00", UpdateUserRequest{ServerMode: "all", RecommendedServerID: &fi, RecommendationPinned: tc.pin}, r)
		})
		if job.Status != "success" || recommendationPinned(app.store.Snapshot().Users[0]) != tc.want {
			t.Fatal(job)
		}
	}
	reopened, err := NewStore(app.store.path)
	if err != nil {
		t.Fatal(err)
	}
	if !recommendationPinned(reopened.Snapshot().Users[0]) || reopened.Snapshot().Servers[0].MonthlyLimitGB != 2000 {
		t.Fatal("settings lost on reload")
	}
}
func TestBalanceRequiresAuthenticationAndRejectsTamperedPlan(t *testing.T) {
	app := newTestApp(t, balanceFixture(2))
	if w := balanceHTTP(t, app, http.MethodPost, "/api/balance/apply", `{"plan_id":"unknown","moves":[]}`); w.Code != 400 {
		t.Fatal(w.Code)
	}
	app.store.Update(func(s *State) error { s.Settings.PasswordHash = "enabled"; return nil })
	for _, path := range []string{"/api/balance", "/api/balance/preview", "/api/balance/apply"} {
		method := http.MethodPost
		if path == "/api/balance" {
			method = http.MethodGet
		}
		if w := balanceHTTP(t, app, method, path, "{}"); w.Code != 401 {
			t.Fatal(path, w.Code)
		}
	}
}

func TestBalanceBusyAndFailedSaveDoNotChangeRecommendations(t *testing.T) {
	app := newTestApp(t, balanceFixture(4))
	plan := balancePreview(t, app)
	before := app.store.Snapshot()
	app.operationMu.Lock()
	if w := balanceHTTP(t, app, http.MethodPost, "/api/balance/preview", "{}"); w.Code != 409 {
		t.Fatal(w.Code)
	}
	if w := balanceApply(t, app, plan.ID); w.Code != 409 {
		t.Fatal(w.Code)
	}
	app.operationMu.Unlock()
	app.store.path = filepath.Join(t.TempDir(), "missing", "master.json")
	if w := balanceApply(t, app, plan.ID); w.Code != 500 {
		t.Fatal(w.Code, w.Body.String())
	}
	if !reflect.DeepEqual(before, app.store.Snapshot()) {
		t.Fatal("failed save changed state")
	}
}
func TestBalanceUnknownUsersUseMedian(t *testing.T) {
	state := balanceFixture(3)
	m := emptyBalanceMetrics()
	now := balanceNow()
	m.StartedAt = now.Add(-24 * time.Hour)
	m.Recent["u00"], m.Recent["u01"] = 2e9, 6e9
	p := buildBalance(state, m, now, true)
	if p.BaselineDailyBytes != 4e9 {
		t.Fatal(p.BaselineDailyBytes)
	}
}
