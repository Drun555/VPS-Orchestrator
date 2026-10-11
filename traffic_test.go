package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func testTraffic(t *testing.T) *Traffic {
	t.Helper()
	v, e := NewTraffic(filepath.Join(t.TempDir(), "traffic.json"), nil)
	if e != nil {
		t.Fatal(e)
	}
	return v
}
func sample(epoch string, up, down int64) TrafficSample {
	return TrafficSample{Epoch: epoch, Counters: map[string]TrafficBytes{"Иван": {up, down}}}
}
func trafficTotal(v *Traffic) TrafficBytes {
	b := TrafficBytes{}
	for _, r := range v.disk.Buckets {
		b.Upload += r.Upload
		b.Download += r.Download
	}
	return b
}

func TestTrafficDurableDeltaResetAndRetention(t *testing.T) {
	tr := testTraffic(t)
	s := Server{ID: "s", DisplayName: "VPS"}
	users := []User{{ID: "u", Email: "Иван"}}
	now := time.Now().UTC().Truncate(time.Second)
	tr.record(s, users, sample("a", 100, 200), now) // Ignore traffic from before monitoring.
	tr.record(s, users, sample("a", 150, 400), now.Add(5*time.Second))
	if b := trafficTotal(tr); b != (TrafficBytes{50, 200}) {
		t.Fatal(b)
	}
	if rate := tr.live["s"].Rates["u"]; rate != (trafficRate{10, 40}) {
		t.Fatal(rate)
	}
	if err := tr.flush(); err != nil {
		t.Fatal(err)
	}
	stat, _ := os.Stat(tr.path)
	if stat.Mode().Perm() != 0600 {
		t.Fatal(stat.Mode())
	}
	restored, err := NewTraffic(tr.path, nil)
	if err != nil {
		t.Fatal(err)
	}
	restored.record(s, users, sample("a", 200, 500), now.Add(10*time.Second))
	if b := trafficTotal(restored); b != (TrafficBytes{100, 300}) {
		t.Fatal("double count after restart", b)
	}
	restored.record(s, users, sample("b", 30, 50), now.Add(15*time.Second))
	if b := trafficTotal(restored); b != (TrafficBytes{130, 350}) {
		t.Fatal("reset", b)
	}
	if restored.live["s"].Status != "warming" || restored.disk.Gaps != 1 {
		t.Fatal("reset masquerades as a rate")
	}
	restored.record(s, users, sample("b", 40, 70), now.Add(20*time.Second))
	// Duplicate and out-of-order samples cannot alter totals.
	restored.record(s, users, sample("a", 9999, 9999), now)
	if b := trafficTotal(restored); b != (TrafficBytes{140, 370}) {
		t.Fatal(b)
	}
	// Reusing a display name for a new identity must not inherit its counter.
	restored.record(s, []User{{ID: "new", Email: "Иван"}}, sample("b", 80, 90), now.Add(25*time.Second))
	if b := trafficTotal(restored); b != (TrafficBytes{140, 370}) {
		t.Fatal("reused name", b)
	}
	restored.disk.Buckets["old"] = trafficBucket{Day: now.AddDate(0, 0, -63), TrafficBytes: TrafficBytes{100, 100}}
	restored.prune(now)
	if _, ok := restored.disk.Buckets["old"]; ok {
		t.Fatal("retention failed")
	}
	// Corrupt history must never silently start from zero.
	os.WriteFile(tr.path, []byte("broken"), 0600)
	if _, err := NewTraffic(tr.path, nil); err == nil {
		t.Fatal("accepted corruption")
	}
}
func TestTrafficPeriodAndMissingSamples(t *testing.T) {
	now := time.Date(2026, 3, 1, 0, 0, 5, 0, time.UTC)
	start, end, err := trafficPeriod("previous", now)
	if err != nil || start.Day() != 1 || start.Month() != 2 || end.Month() != 3 {
		t.Fatal(start, end, err)
	}
	tr := testTraffic(t)
	s := Server{ID: "s"}
	u := []User{{ID: "u", Email: "Иван"}}
	tr.record(s, u, sample("a", 0, 0), now.Add(-10*time.Second))
	tr.record(s, u, sample("a", 10, 100), now.Add(-6*time.Second))
	tr.record(s, u, sample("a", 20, 200), now)
	state := State{Servers: []Server{s}, Users: u}
	view := tr.view(state, start, end, now)
	if len(view.Rows) != 1 || view.Rows[0].Download != 100 {
		t.Fatal(view.Rows)
	}
	// Accumulated totals after an outage are retained, but are not a live rate.
	tr.record(s, u, sample("a", 30, 400), now.Add(time.Hour))
	if tr.live["s"].Status != "warming" {
		t.Fatal("hourly average presented as live")
	}
	tr.record(s, u, sample("a", 40, 450), now.Add(time.Hour+5*time.Second))
	view = tr.view(state, start, now.Add(2*time.Hour), now.Add(2*time.Hour))
	if view.Rows[0].Rate != nil || view.Servers[0].Status != "stale" {
		t.Fatal("stale speed")
	}
	if _, _, err := trafficPeriod("bad", now); err == nil {
		t.Fatal("bad period")
	}
}
func TestParseTrafficCounters(t *testing.T) {
	raw := []byte(`{"stat":[{"name":"user>>>имя>>>traffic>>>uplink","value":"123"},{"name":"user>>>имя>>>traffic>>>downlink","value":456},{"name":"user>>>zero>>>traffic>>>uplink"},{"name":"inbound>>>vless>>>traffic>>>uplink","value":999}]}`)
	counters, err := parseTrafficCounters(raw)
	if err != nil || counters["имя"] != (TrafficBytes{123, 456}) || len(counters) != 2 {
		t.Fatal(counters, err)
	}
	for _, raw := range []string{`{"stat":[{"name":"user>>>u>>>traffic>>>uplink","value":"-1"}]}`, `{"stat":[{"name":"user>>>u>>>traffic>>>uplink","value":"9999999999999999999999999"}]}`, `garbage`} {
		if _, err := parseTrafficCounters([]byte(raw)); err == nil {
			t.Fatal(raw)
		}
	}
}
func TestTrafficConfigPreservesPolicyAndRejectsForeignAPI(t *testing.T) {
	var root map[string]json.RawMessage
	json.Unmarshal([]byte(`{"policy":{"levels":{"0":{"handshake":42},"1":{"bufferSize":8}},"system":{"statsInboundUplink":true}},"routing":{"domainStrategy":"AsIs"}}`), &root)
	if err := enableTrafficConfig(root); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(root)
	for _, s := range []string{`"handshake":42`, `"bufferSize":8`, `"statsInboundUplink":true`, `"domainStrategy":"AsIs"`, `"statsUserDownlink":true`, `127.0.0.1:10085`} {
		if !strings.Contains(string(raw), s) {
			t.Fatal(s)
		}
	}
	if err := enableTrafficConfig(root); err != nil {
		t.Fatal(err)
	}
	raw2, _ := json.Marshal(root)
	if string(raw) != string(raw2) {
		t.Fatal("not idempotent")
	}
	root["api"] = json.RawMessage(`{"listen":"0.0.0.0:2000","services":["HandlerService"]}`)
	if err := enableTrafficConfig(root); err == nil {
		t.Fatal("foreign API overwritten")
	}
}
func TestTrafficAPIAuthenticationAndDiskFailure(t *testing.T) {
	app := newTestApp(t, State{Version: stateVersion, Settings: Settings{PasswordHash: "set"}})
	app.traffic = testTraffic(t)
	w := httptest.NewRecorder()
	app.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/traffic", nil))
	if w.Code != 401 {
		t.Fatal(w.Code)
	}
	app.traffic.path = filepath.Join(t.TempDir(), "missing", "traffic.json")
	if err := app.traffic.flush(); err == nil || app.traffic.saveError == "" {
		t.Fatal("disk error concealed")
	}
}

type blockingTrafficSource struct{ entered chan string }

func (s blockingTrafficSource) Read(ctx context.Context, server Server) (TrafficSample, error) {
	select {
	case s.entered <- server.ID:
	default:
	}
	<-ctx.Done()
	return TrafficSample{}, ctx.Err()
}
func TestTrafficCollectorCancellationAndConcurrency(t *testing.T) {
	tr := testTraffic(t)
	source := blockingTrafficSource{make(chan string, 10)}
	tr.source = source
	app := newTestApp(t, State{Version: stateVersion, Servers: []Server{{ID: "1"}, {ID: "2"}, {ID: "3"}, {ID: "4"}, {ID: "5"}}})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { tr.Run(ctx, app.store); close(done) }()
	for i := 0; i < 4; i++ {
		select {
		case <-source.entered:
		case <-time.After(3 * time.Second):
			t.Fatal("collector failed to start workers")
		}
	}
	select {
	case <-source.entered:
		t.Fatal("more than four SSH sessions")
	case <-time.After(50 * time.Millisecond):
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("shutdown hangs")
	}
	if _, err := os.Stat(tr.path); err != nil {
		t.Fatal("shutdown did not persist", err)
	}
}
func TestTrafficConcurrentReaders(t *testing.T) {
	tr := testTraffic(t)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for n := 0; n < 10; n++ {
				now := time.Now()
				tr.record(Server{ID: "s"}, []User{{ID: "u", Email: "Иван"}}, sample("a", 1, 1), now)
				tr.view(State{}, now.Add(-time.Hour), now, now)
			}
		}()
	}
	wg.Wait()
}

func TestTrafficAPIResponseHasTotalsWithoutCredentials(t *testing.T) {
	app := newTestApp(t, State{Version: stateVersion, Servers: []Server{{ID: "s", SSHPrivateKey: "secret-key", DuckDNSToken: "secret-token"}}, Users: []User{{ID: "u", Email: "Иван", SubscriptionToken: "secret-subscription"}}})
	app.traffic = testTraffic(t)
	now := time.Now().UTC()
	state := app.store.Snapshot()
	app.traffic.record(state.Servers[0], state.Users, sample("epoch", 0, 0), now.Add(-5*time.Second))
	app.traffic.record(state.Servers[0], state.Users, sample("epoch", 100, 200), now)
	w := httptest.NewRecorder()
	app.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/traffic?period=month", nil))
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	for _, secret := range []string{"secret-key", "secret-token", "secret-subscription"} {
		if strings.Contains(w.Body.String(), secret) {
			t.Fatal("secret exposed")
		}
	}
	var view trafficView
	if err := json.Unmarshal(w.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if len(view.Rows) != 1 || view.Rows[0].Upload != 100 || view.Rows[0].Rate == nil {
		t.Fatal(view)
	}
	w = httptest.NewRecorder()
	app.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/traffic?period=invalid", nil))
	if w.Code != 400 {
		t.Fatal(w.Code)
	}
}
