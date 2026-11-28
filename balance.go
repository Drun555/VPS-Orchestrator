package main

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"sort"
	"time"
)

const balancePlanTTL = 15 * time.Minute

var errBalanceConflict = errors.New("План больше не актуален")

func balanceWeight(s Server) int {
	if s.BalanceWeight < 1 {
		return 1
	}
	return s.BalanceWeight
}

func recommendationPinned(u User) bool {
	if u.RecommendedServerID == "" {
		return false
	}
	if u.RecommendationPinned != nil {
		return *u.RecommendationPinned
	}
	// Preserve recommendations selected manually before balancing was introduced.
	return true
}

// Match the first usable entry in the client's provider, including its fallback
// when a preferred server is no longer allowed or has an invalid link.
func balanceCurrentServer(state State, u User) string {
	first := ""
	for _, s := range state.Servers {
		if !balanceLinkReady(u, s) {
			continue
		}
		if s.ID == u.RecommendedServerID {
			return s.ID
		}
		if first == "" {
			first = s.ID
		}
	}
	return first
}
func balanceLinkReady(u User, s Server) bool {
	link := u.Links[s.ID]
	if !userAllowsServer(u, s) || link.Status != "ready" {
		return false
	}
	_, err := parseVLESSReality(link.URI, serverDisplayName(s))
	return err == nil
}

type balanceMetrics struct {
	Month     map[string]float64
	Recent    map[string]float64
	Live      map[string]trafficLive
	StartedAt time.Time
	Gaps      int
	SaveError string
}

func (app *App) balanceMetrics(now time.Time) balanceMetrics {
	m := balanceMetrics{Month: map[string]float64{}, Recent: map[string]float64{}, Live: map[string]trafficLive{}}
	if app.traffic == nil {
		return m
	}
	t := app.traffic
	t.mu.Lock()
	defer t.mu.Unlock()
	t.activeUntil = now.Add(15 * time.Second)
	m.StartedAt, m.Gaps, m.SaveError = t.disk.StartedAt, t.disk.Gaps, t.saveError
	month := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	recent := now.UTC().Truncate(24*time.Hour).AddDate(0, 0, -29)
	for _, b := range t.disk.Buckets {
		if b.Day.After(now) {
			continue
		}
		bytes := float64(b.Upload) + float64(b.Download)
		if !b.Day.Before(month) {
			m.Month[b.ServerID] += bytes
		}
		if !b.Day.Before(recent) {
			m.Recent[b.UserID] += bytes
		}
	}
	for id, v := range t.live {
		copy := v
		copy.Rates = map[string]trafficRate{}
		for user, rate := range v.Rates {
			copy.Rates[user] = rate
		}
		if copy.Status == "ready" && now.Sub(copy.At) > 15*time.Second {
			copy.Status = "stale"
		}
		m.Live[id] = copy
	}
	return m
}

type balanceServerView struct {
	ID              string       `json:"id"`
	Name            string       `json:"name"`
	Weight          int          `json:"weight"`
	Disabled        bool         `json:"disabled"`
	MonthlyLimitGB  int64        `json:"monthly_limit_gb"`
	MonthBytes      float64      `json:"month_bytes"`
	Rate            *trafficRate `json:"rate"`
	TelemetryStatus string       `json:"telemetry_status"`
	UpdatedAt       time.Time    `json:"updated_at"`
	ExcludedReason  string       `json:"excluded_reason,omitempty"`
	UsersBefore     int          `json:"users_before"`
	UsersAfter      int          `json:"users_after"`
	PinnedUsers     int          `json:"pinned_users"`
	DailyBefore     float64      `json:"daily_before"`
	DailyAfter      float64      `json:"daily_after"`
	ForecastBytes   float64      `json:"forecast_bytes"`
}
type balanceMove struct {
	UserID     string  `json:"user_id"`
	UserName   string  `json:"user_name"`
	From       string  `json:"from"`
	To         string  `json:"to"`
	DailyBytes float64 `json:"daily_bytes"`
	Estimated  bool    `json:"estimated"`
}
type balanceSkipped struct {
	UserID   string `json:"user_id"`
	UserName string `json:"user_name"`
	Reason   string `json:"reason"`
}
type balanceView struct {
	ID                 string              `json:"plan_id,omitempty"`
	ExpiresAt          time.Time           `json:"expires_at,omitempty"`
	CreatedAt          time.Time           `json:"created_at"`
	HistoryFrom        time.Time           `json:"history_from"`
	BaselineDailyBytes float64             `json:"baseline_daily_bytes"`
	Servers            []balanceServerView `json:"servers"`
	Moves              []balanceMove       `json:"moves"`
	Skipped            []balanceSkipped    `json:"skipped"`
	Warnings           []string            `json:"warnings"`
}
type balancePlan struct {
	View     balanceView
	Revision [32]byte
}

func balanceRevision(state State) [32]byte {
	data, _ := json.Marshal(state)
	return sha256.Sum256(data)
}
func balanceExcluded(s Server, m balanceMetrics) string {
	switch {
	case s.BalanceDisabled:
		return "Исключён из балансировки"
	case s.Provisioning:
		return "Установка не завершена"
	case s.Status != "ready":
		return "Сервер не готов"
	case m.Live[s.ID].Status == "offline":
		return "Ошибка связи со сборщиком статистики"
	case s.MonthlyLimitGB > 0 && m.Month[s.ID] >= float64(s.MonthlyLimitGB)*1e9:
		return "Месячный лимит достигнут"
	}
	return ""
}

func buildBalance(state State, m balanceMetrics, now time.Time, preview bool) balanceView {
	now = now.UTC()
	from := now.Truncate(24*time.Hour).AddDate(0, 0, -29)
	if m.StartedAt.After(from) {
		from = m.StartedAt
	}
	days := math.Max(1, now.Sub(from).Hours()/24)
	remainingDays := time.Date(now.Year(), now.Month()+1, 1, 0, 0, 0, 0, time.UTC).Sub(now).Hours() / 24
	v := balanceView{CreatedAt: now, HistoryFrom: from, Servers: []balanceServerView{}, Moves: []balanceMove{}, Skipped: []balanceSkipped{}, Warnings: []string{}}
	demand := map[string]float64{}
	var measured []float64
	for _, u := range state.Users {
		if bytes := m.Recent[u.ID]; bytes > 0 {
			demand[u.ID] = bytes / days
			measured = append(measured, bytes/days)
		}
	}
	baseline := 1e9 // Only a planning estimate, never presented as measured traffic.
	if len(measured) > 0 {
		sort.Float64s(measured)
		baseline = measured[len(measured)/2]
		if len(measured)%2 == 0 {
			baseline = (baseline + measured[len(measured)/2-1]) / 2
		}
	}
	v.BaselineDailyBytes = baseline
	if len(measured) < len(state.Users) {
		v.Warnings = append(v.Warnings, "Для пользователей без трафика за период применяется оценка: медиана активных пользователей, а без истории — 1 ГБ/день.")
	}
	month := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	if m.StartedAt.IsZero() || m.StartedAt.After(month) || m.Gaps > 0 {
		v.Warnings = append(v.Warnings, "История месяца неполная. Лимиты учитывают только записанный VPN-трафик, без системного трафика VPS; сверьте остаток с данными хостера.")
	}
	if m.SaveError != "" {
		v.Warnings = append(v.Warnings, m.SaveError)
	}
	rows := map[string]*balanceServerView{}
	for _, s := range state.Servers {
		live := m.Live[s.ID]
		status := live.Status
		if status == "" {
			status = "waiting"
		}
		row := balanceServerView{ID: s.ID, Name: serverDisplayName(s), Weight: balanceWeight(s), Disabled: s.BalanceDisabled, MonthlyLimitGB: s.MonthlyLimitGB, MonthBytes: m.Month[s.ID], TelemetryStatus: status, UpdatedAt: live.At, ExcludedReason: balanceExcluded(s, m)}
		if status == "ready" {
			rate := trafficRate{}
			for _, r := range live.Rates {
				rate.Upload += r.Upload
				rate.Download += r.Download
			}
			row.Rate = &rate
		}
		v.Servers = append(v.Servers, row)
	}
	for i := range v.Servers {
		rows[v.Servers[i].ID] = &v.Servers[i]
	}
	type candidate struct {
		User    User
		Current string
		Demand  float64
		Options []string
	}
	var candidates []candidate
	for _, u := range state.Users {
		d := demand[u.ID]
		if d == 0 {
			d = baseline
		}
		current := balanceCurrentServer(state, u)
		if row := rows[current]; row != nil {
			row.UsersBefore++
			row.DailyBefore += d
			row.UsersAfter++
			row.DailyAfter += d
		}
		c := candidate{User: u, Current: current, Demand: d}
		if !recommendationPinned(u) {
			for _, s := range state.Servers {
				if rows[s.ID].ExcludedReason == "" && balanceLinkReady(u, s) {
					c.Options = append(c.Options, s.ID)
				}
			}
		}
		switch {
		case recommendationPinned(u):
			if row := rows[current]; row != nil {
				row.PinnedUsers++
			}
			v.Skipped = append(v.Skipped, balanceSkipped{u.ID, u.Email, "Рекомендация закреплена вручную"})
		case !preview:
		case len(c.Options) == 0:
			v.Skipped = append(v.Skipped, balanceSkipped{u.ID, u.Email, "Нет доступного сервера для балансировки"})
		default:
			candidates = append(candidates, c)
		}
	}
	if preview {
		v.Warnings = append(v.Warnings, "Сначала выравнивается число пользователей с учётом весов серверов; при равном числе назначений учитывается трафик. Неактивные пользователи тоже занимают место.")
	}
	// Reserve existing load, including clients that cannot move. Reconsider the
	// most restricted clients first, then the heaviest; preserve their node on ties.
	sort.Slice(candidates, func(i, j int) bool {
		a, b := candidates[i], candidates[j]
		if len(a.Options) != len(b.Options) {
			return len(a.Options) < len(b.Options)
		}
		if a.Demand != b.Demand {
			return a.Demand > b.Demand
		}
		return a.User.ID < b.User.ID
	})
	for _, c := range candidates {
		if row := rows[c.Current]; row != nil {
			row.UsersAfter--
			row.DailyAfter -= c.Demand
			if row.DailyAfter < 0 {
				row.DailyAfter = 0
			}
		}
		best := ""
		userScore, trafficScore := math.Inf(1), math.Inf(1)
		for _, id := range c.Options {
			row := rows[id]
			projected := row.DailyAfter + c.Demand
			if row.MonthlyLimitGB > 0 && row.MonthBytes+projected*remainingDays > float64(row.MonthlyLimitGB)*1e9 {
				continue
			}
			// Every account reserves a weighted slot, even with no traffic.
			// Use consumption to choose among equally occupied servers.
			nUsers := float64(row.UsersAfter+1) / float64(row.Weight)
			nTraffic := projected / float64(row.Weight)
			if nUsers < userScore || (nUsers == userScore && (nTraffic < trafficScore || (nTraffic == trafficScore && id == c.Current))) {
				best, userScore, trafficScore = id, nUsers, nTraffic
			}
		}
		if best == "" {
			best = c.Current
			v.Skipped = append(v.Skipped, balanceSkipped{c.User.ID, c.User.Email, "Недостаточно месячного остатка по прогнозу; рекомендация сохранена"})
		}
		if row := rows[best]; row != nil {
			row.UsersAfter++
			row.DailyAfter += c.Demand
		}
		if best != "" && best != c.Current {
			v.Moves = append(v.Moves, balanceMove{c.User.ID, c.User.Email, c.Current, best, c.Demand, demand[c.User.ID] == 0})
		}
	}
	for i := range v.Servers {
		row := &v.Servers[i]
		row.ForecastBytes = row.MonthBytes + row.DailyAfter*remainingDays
		if row.MonthlyLimitGB > 0 && row.ForecastBytes > float64(row.MonthlyLimitGB)*1e9 {
			v.Warnings = append(v.Warnings, fmt.Sprintf("%s: прогноз превышает месячный лимит; часть пользователей осталась на текущем сервере.", row.Name))
		}
	}
	return v
}

func (app *App) handleBalance(w http.ResponseWriter, r *http.Request) {
	now := time.Now().UTC()
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, 200, buildBalance(app.store.Snapshot(), app.balanceMetrics(now), now, false))
}
func (app *App) handleBalancePreview(w http.ResponseWriter, r *http.Request) {
	var input struct{}
	if decodeJSON(w, r, &input) != nil {
		return
	}
	if !app.operationMu.TryLock() {
		writeError(w, 409, "Дождитесь завершения текущей операции и рассчитайте план заново.")
		return
	}
	defer app.operationMu.Unlock()
	now := time.Now().UTC()
	state := app.store.Snapshot()
	v := buildBalance(state, app.balanceMetrics(now), now, true)
	id, err := randomToken()
	if err != nil {
		writeError(w, 500, "Не удалось создать план")
		return
	}
	v.ID, v.ExpiresAt = id, now.Add(balancePlanTTL)
	app.balanceMu.Lock()
	if app.balancePlans == nil {
		app.balancePlans = map[string]balancePlan{}
	}
	for key, plan := range app.balancePlans {
		if !now.Before(plan.View.ExpiresAt) {
			delete(app.balancePlans, key)
		}
	}
	if len(app.balancePlans) >= 32 {
		oldest := ""
		var at time.Time
		for key, plan := range app.balancePlans {
			if oldest == "" || plan.View.CreatedAt.Before(at) {
				oldest, at = key, plan.View.CreatedAt
			}
		}
		delete(app.balancePlans, oldest)
	}
	app.balancePlans[id] = balancePlan{v, balanceRevision(state)}
	app.balanceMu.Unlock()
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, 200, v)
}
func (app *App) handleBalanceApply(w http.ResponseWriter, r *http.Request) {
	var input struct {
		ID string `json:"plan_id"`
	}
	if decodeJSON(w, r, &input) != nil {
		return
	}
	if !app.operationMu.TryLock() {
		writeError(w, 409, "Дождитесь завершения текущей операции перед применением плана.")
		return
	}
	defer app.operationMu.Unlock()
	app.balanceMu.Lock()
	plan, ok := app.balancePlans[input.ID]
	app.balanceMu.Unlock()
	now := time.Now().UTC()
	if !ok || !now.Before(plan.View.ExpiresAt) {
		writeError(w, 409, "План устарел. Рассчитайте балансировку заново.")
		return
	}
	metrics := app.balanceMetrics(now)
	if plan.View.CreatedAt.Year() != now.Year() || plan.View.CreatedAt.Month() != now.Month() {
		writeError(w, 409, "Начался новый месяц. Рассчитайте план заново.")
		return
	}
	err := app.store.Update(func(state *State) error {
		if balanceRevision(*state) != plan.Revision {
			return fmt.Errorf("%w: настройки или назначения изменились. Рассчитайте план заново.", errBalanceConflict)
		}
		for _, move := range plan.View.Moves {
			u, _ := findUser(state, move.UserID)
			s, _ := findServer(state, move.To)
			if u == nil || s == nil || recommendationPinned(*u) || !balanceLinkReady(*u, *s) || balanceExcluded(*s, metrics) != "" {
				return fmt.Errorf("%w: состояние сервера или пользователя изменилось. Рассчитайте план заново.", errBalanceConflict)
			}
			for _, row := range plan.View.Servers {
				remaining := time.Date(now.Year(), now.Month()+1, 1, 0, 0, 0, 0, time.UTC).Sub(now).Hours() / 24
				if row.ID == move.To && s.MonthlyLimitGB > 0 && metrics.Month[s.ID]+row.DailyAfter*remaining > float64(s.MonthlyLimitGB)*1e9 {
					return fmt.Errorf("%w: изменился остаток месячного лимита. Рассчитайте план заново.", errBalanceConflict)
				}
			}
		}
		for _, move := range plan.View.Moves {
			u, _ := findUser(state, move.UserID)
			unpinned := false
			u.RecommendedServerID = move.To
			u.RecommendationPinned = &unpinned
			u.UpdatedAt = now
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, errBalanceConflict) {
			writeError(w, 409, err.Error())
		} else {
			writeError(w, 500, "Не удалось сохранить рекомендации")
		}
		return
	}
	app.balanceMu.Lock()
	delete(app.balancePlans, input.ID)
	app.balanceMu.Unlock()
	writeJSON(w, 200, map[string]any{"applied": len(plan.View.Moves), "message": "Рекомендации обновлены. Обновите список серверов в VPN-клиентах; автоматически он обновляется примерно раз в час."})
}
