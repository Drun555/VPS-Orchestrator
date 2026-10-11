package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

type trafficCounter struct {
	UserID string `json:"user_id"`
	TrafficBytes
}
type trafficCursor struct {
	Epoch    string                    `json:"epoch"`
	At       time.Time                 `json:"at"`
	Counters map[string]trafficCounter `json:"counters"`
}
type trafficBucket struct {
	Day        time.Time `json:"day"`
	ServerID   string    `json:"server_id"`
	UserID     string    `json:"user_id"`
	ServerName string    `json:"server_name"`
	UserName   string    `json:"user_name"`
	TrafficBytes
}
type trafficDisk struct {
	Version   int                      `json:"version"`
	StartedAt time.Time                `json:"started_at"`
	Cursors   map[string]trafficCursor `json:"cursors"`
	Buckets   map[string]trafficBucket `json:"buckets"`
	Gaps      int                      `json:"gaps"`
}
type trafficRate struct {
	Upload   float64 `json:"upload"`
	Download float64 `json:"download"`
}
type trafficLive struct {
	Status string                 `json:"status"`
	At     time.Time              `json:"updated_at"`
	Rates  map[string]trafficRate `json:"-"`
}
type Traffic struct {
	sampleLocks sync.Map
	mu          sync.Mutex
	path        string
	disk        trafficDisk
	live        map[string]trafficLive
	activeUntil time.Time
	saveError   string
	source      TrafficSource
}

func NewTraffic(path string, source TrafficSource) (*Traffic, error) {
	t := &Traffic{path: path, source: source, live: map[string]trafficLive{}, disk: trafficDisk{Version: 1, StartedAt: time.Now().UTC(), Cursors: map[string]trafficCursor{}, Buckets: map[string]trafficBucket{}}}
	raw, err := os.ReadFile(path)
	if err == nil {
		if err = json.Unmarshal(raw, &t.disk); err != nil {
			return nil, fmt.Errorf("read traffic history: %w", err)
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	if t.disk.Version != 1 || t.disk.Cursors == nil || t.disk.Buckets == nil {
		return nil, fmt.Errorf("unsupported traffic history")
	}
	return t, nil
}

func (t *Traffic) record(server Server, users []User, sample TrafficSample, now time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	old, exists := t.disk.Cursors[server.ID]
	if exists && !now.After(old.At) {
		return
	}
	reset := exists && old.Epoch != sample.Epoch
	if reset {
		t.disk.Gaps++
	} // Any unsampled tail of the old process is unknowable.
	cursor := trafficCursor{Epoch: sample.Epoch, At: now, Counters: map[string]trafficCounter{}}
	live := trafficLive{Status: "warming", At: now, Rates: map[string]trafficRate{}}
	seconds := now.Sub(old.At).Seconds()
	realtime := exists && !reset && seconds > 0 && seconds <= 15
	if realtime {
		live.Status = "ready"
	}
	byName := map[string]User{}
	for _, u := range users {
		byName[u.Email] = u
	}
	for name, counter := range old.Counters {
		if _, ok := byName[name]; !ok {
			byName[name] = User{ID: counter.UserID, Email: name}
		}
	}
	for name, b := range sample.Counters {
		u, ok := byName[name]
		if !ok {
			continue
		}
		cursor.Counters[name] = trafficCounter{UserID: u.ID, TrafficBytes: b}
		previous, had := old.Counters[name]
		delta := TrafficBytes{}
		if exists {
			if reset {
				delta = b
			} else if had && previous.UserID == u.ID {
				delta.Upload = b.Upload - previous.Upload
				delta.Download = b.Download - previous.Download
				if delta.Upload < 0 || delta.Download < 0 {
					t.disk.Gaps++
					delta = TrafficBytes{}
					live.Status = "warming"
					realtime = false
				}
			} else if !had { // A newly registered Xray counter starts at zero.
				delta = b
			}
		}
		if realtime {
			live.Rates[u.ID] = trafficRate{float64(delta.Upload) / seconds, float64(delta.Download) / seconds}
		}
		if delta.Upload != 0 || delta.Download != 0 {
			// Counters have no timestamps: attribute a delta to the collection day.
			day := now.UTC().Truncate(24 * time.Hour)
			key := day.Format(time.RFC3339) + "/" + server.ID + "/" + u.ID
			bucket := t.disk.Buckets[key]
			bucket.Day = day
			bucket.ServerID = server.ID
			bucket.UserID = u.ID
			bucket.ServerName = serverDisplayName(server)
			bucket.UserName = u.Email
			bucket.Upload += delta.Upload
			bucket.Download += delta.Download
			t.disk.Buckets[key] = bucket
		}
	}
	if !realtime {
		live.Rates = map[string]trafficRate{}
	}
	t.disk.Cursors[server.ID] = cursor
	t.live[server.ID] = live
	t.prune(now)
}
func (t *Traffic) prune(now time.Time) {
	cutoff := now.UTC().AddDate(0, 0, -62)
	for k, b := range t.disk.Buckets {
		if b.Day.Before(cutoff) {
			delete(t.disk.Buckets, k)
		}
	}
	for k, c := range t.disk.Cursors {
		if c.At.Before(cutoff) {
			delete(t.disk.Cursors, k)
		}
	}
}

func (t *Traffic) flush() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.prune(time.Now())
	err := writeTrafficFile(t.path, t.disk)
	t.saveError = ""
	if err != nil {
		t.saveError = "Не удалось сохранить историю на диск. Проверьте доступное место и права DATA_DIR."
	}
	return err
}
func writeTrafficFile(path string, data trafficDisk) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".traffic-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	if err = json.NewEncoder(f).Encode(data); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Rename(f.Name(), path); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func (t *Traffic) sampleLock(id string) *sync.Mutex {
	lock, _ := t.sampleLocks.LoadOrStore(id, &sync.Mutex{})
	return lock.(*sync.Mutex)
}

func (t *Traffic) captureRemote(r remoteCommand, server Server, users []User) {
	lock := t.sampleLock(server.ID)
	lock.Lock()
	defer lock.Unlock()
	sample, err := readTrafficRemote(r)
	if err == nil {
		t.record(server, users, sample, time.Now().UTC())
		_ = t.flush()
	}
}

func (t *Traffic) collect(ctx context.Context, server Server, users []User) {
	lock := t.sampleLock(server.ID)
	lock.Lock()
	defer lock.Unlock()
	if ctx.Err() != nil {
		return
	}
	sample, err := t.source.Read(ctx, server)
	if err == nil {
		t.record(server, users, sample, time.Now().UTC())
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	status := "offline"
	if err == errStatsDisabled {
		status = "disabled"
	}
	previous := t.live[server.ID]
	previous.Status = status
	previous.Rates = nil
	t.live[server.ID] = previous
}

// One scheduler for all browsers. Slow/offline VPS do not block healthy VPS;
// at most four SSH sessions are outstanding and each has a ten-second deadline.
func (t *Traffic) Run(ctx context.Context, store *Store) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	flushTicker := time.NewTicker(time.Minute)
	defer flushTicker.Stop()
	var workers sync.WaitGroup
	busy := sync.Map{}
	slots := make(chan struct{}, 4)
	next := map[string]time.Time{}
	defer func() { workers.Wait(); _ = t.flush() }()
	for {
		select {
		case <-ctx.Done():
			return
		case <-flushTicker.C:
			_ = t.flush()
		case now := <-ticker.C:
			t.mu.Lock()
			interval := time.Minute
			if now.Before(t.activeUntil) {
				interval = 5 * time.Second
			}
			t.mu.Unlock()
			state := store.Snapshot()
			sort.SliceStable(state.Servers, func(i, j int) bool { return next[state.Servers[i].ID].Before(next[state.Servers[j].ID]) })
			for _, server := range state.Servers {
				if server.Provisioning {
					continue
				}
				// Recompute from the last scheduled time so opening the UI accelerates polling immediately.
				if last, ok := next[server.ID]; ok && now.Sub(last) < interval {
					continue
				}
				if _, loaded := busy.LoadOrStore(server.ID, true); loaded {
					continue
				}
				select {
				case slots <- struct{}{}:
				default:
					busy.Delete(server.ID)
					continue
				}
				next[server.ID] = now
				workers.Add(1)
				go func(s Server) {
					defer workers.Done()
					defer func() { <-slots; busy.Delete(s.ID) }()
					t.collect(ctx, s, state.Users)
				}(server)
			}
		}
	}
}

type trafficRow struct {
	ServerID   string `json:"server_id"`
	ServerName string `json:"server_name"`
	UserID     string `json:"user_id"`
	UserName   string `json:"user_name"`
	TrafficBytes
	Rate *trafficRate `json:"rate"`
}
type trafficServerView struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	trafficLive
}
type trafficView struct {
	StartedAt time.Time           `json:"started_at"`
	From      time.Time           `json:"from"`
	To        time.Time           `json:"to"`
	Rows      []trafficRow        `json:"rows"`
	Servers   []trafficServerView `json:"servers"`
	Gaps      int                 `json:"gaps"`
	SaveError string              `json:"save_error,omitempty"`
}

func trafficPeriod(period string, now time.Time) (time.Time, time.Time, error) {
	now = now.UTC()
	month := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	switch period {
	case "", "month":
		return month, now, nil
	case "previous":
		return month.AddDate(0, -1, 0), month, nil
	case "30days":
		return now.Truncate(24*time.Hour).AddDate(0, 0, -29), now, nil
	default:
		return time.Time{}, time.Time{}, fmt.Errorf("unknown period")
	}
}
func (t *Traffic) view(state State, from, to, now time.Time) trafficView {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.activeUntil = now.Add(15 * time.Second)
	v := trafficView{StartedAt: t.disk.StartedAt, From: from, To: to, Rows: []trafficRow{}, Servers: []trafficServerView{}, Gaps: t.disk.Gaps, SaveError: t.saveError}
	rows := map[string]*trafficRow{}
	for _, b := range t.disk.Buckets {
		if b.Day.Before(from) || !b.Day.Before(to) {
			continue
		}
		key := b.ServerID + "/" + b.UserID
		r := rows[key]
		if r == nil {
			r = &trafficRow{ServerID: b.ServerID, ServerName: b.ServerName, UserID: b.UserID, UserName: b.UserName}
			rows[key] = r
		}
		r.Upload += b.Upload
		r.Download += b.Download
	}
	for _, s := range state.Servers {
		live, ok := t.live[s.ID]
		if !ok {
			live.Status = "waiting"
		}
		if s.Provisioning {
			live.Status = "provisioning"
		}
		if live.Status == "ready" && now.Sub(live.At) > 15*time.Second {
			live.Status = "stale"
		}
		v.Servers = append(v.Servers, trafficServerView{ID: s.ID, Name: serverDisplayName(s), trafficLive: live})
		for _, u := range state.Users {
			key := s.ID + "/" + u.ID
			r := rows[key]
			_, hasLink := u.Links[s.ID]
			if r == nil && !userAllowsServer(u, s) && !hasLink {
				continue
			}
			if r == nil {
				r = &trafficRow{ServerID: s.ID, UserID: u.ID}
				rows[key] = r
			}
			r.ServerName = serverDisplayName(s)
			r.UserName = u.Email
			if live.Status == "ready" {
				rate := live.Rates[u.ID]
				r.Rate = &rate
			}
		}
	}
	for _, r := range rows {
		v.Rows = append(v.Rows, *r)
	}
	return v
}
func (app *App) handleTraffic(w http.ResponseWriter, r *http.Request) {
	if app.traffic == nil {
		writeError(w, http.StatusServiceUnavailable, "Statistics collector is unavailable")
		return
	}
	now := time.Now().UTC()
	from, to, err := trafficPeriod(r.URL.Query().Get("period"), now)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, app.traffic.view(app.store.Snapshot(), from, to, now))
}
