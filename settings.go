package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"
)

type Settings struct {
	BaseURL       string `json:"base_url,omitempty"`
	CertbotEmail  string `json:"certbot_email,omitempty"`
	DuckDNSToken  string `json:"duckdns_token,omitempty"`
	PasswordHash  string `json:"password_hash,omitempty"`
	SessionSecret string `json:"session_secret,omitempty"`
	SetupToken    string `json:"setup_token,omitempty"`
}

func (app *App) settings() Settings {
	s := app.store.Snapshot().Settings
	// Explicit environment/.env configuration retains priority over browser settings.
	if app.config.BaseURL != "" {
		s.BaseURL = app.config.BaseURL
	}
	if app.config.CertbotEmail != "" {
		s.CertbotEmail = app.config.CertbotEmail
	}
	return s
}
func (app *App) needsSetup() bool {
	s := app.settings()
	return s.BaseURL == "" || s.CertbotEmail == ""
}
func (app *App) initializeSettings() error {
	needsSetup := app.needsSetup()
	return app.store.Update(func(state *State) error {
		var err error
		if state.Settings.SessionSecret == "" {
			state.Settings.SessionSecret, err = randomToken()
			if err != nil {
				return err
			}
		}
		if needsSetup && state.Settings.SetupToken == "" {
			state.Settings.SetupToken, err = randomToken()
			if err != nil {
				return err
			}
		}
		if state.Settings.DuckDNSToken == "" && len(state.Servers) > 0 {
			token := state.Servers[0].DuckDNSToken
			for _, server := range state.Servers {
				if server.DuckDNSToken != token {
					token = ""
					break
				}
			}
			state.Settings.DuckDNSToken = token
		}
		return nil
	})
}
func (app *App) authenticated(r *http.Request) bool {
	s := app.settings()
	if s.PasswordHash == "" {
		return !app.needsSetup()
	}
	cookie, err := r.Cookie("orchestrator_session")
	if err != nil {
		return false
	}
	exp, sig, ok := strings.Cut(cookie.Value, ".")
	if !ok {
		return false
	}
	unix, err := strconv.ParseInt(exp, 10, 64)
	if err != nil || time.Now().Unix() >= unix {
		return false
	}
	return hmac.Equal([]byte(sig), []byte(sessionSignature(exp, s.SessionSecret))) && s.SessionSecret != ""
}
func sessionSignature(value, secret string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(value))
	return hex.EncodeToString(mac.Sum(nil))
}
func (app *App) setSession(w http.ResponseWriter) {
	s := app.settings()
	exp := strconv.FormatInt(time.Now().Add(24*time.Hour).Unix(), 10)
	http.SetCookie(w, &http.Cookie{Name: "orchestrator_session", Value: exp + "." + sessionSignature(exp, s.SessionSecret), Path: "/", HttpOnly: true, Secure: strings.HasPrefix(s.BaseURL, "https://"), SameSite: http.SameSiteStrictMode, MaxAge: 86400})
}
func (app *App) authorize(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/api/") {
			next.ServeHTTP(w, r)
			return
		}
		if r.Method != "GET" && r.Method != "HEAD" {
			if origin := r.Header.Get("Origin"); origin != "" {
				parsed, err := url.Parse(origin)
				base, _ := url.Parse(app.settings().BaseURL)
				if err != nil || (parsed.Host != r.Host && (base == nil || parsed.Host != base.Host || parsed.Scheme != base.Scheme)) {
					writeError(w, 403, "cross-origin request rejected")
					return
				}
			}
		}
		public := r.URL.Path == "/api/session" || r.URL.Path == "/api/login" || (app.needsSetup() && r.URL.Path == "/api/settings" && r.Method == "PUT")
		if !public && !app.authenticated(r) {
			writeError(w, 401, "Требуется вход или первоначальная настройка")
			return
		}
		next.ServeHTTP(w, r)
	})
}
func (app *App) handleSession(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{"setup_required": app.needsSetup(), "authenticated": app.authenticated(r), "password_enabled": app.settings().PasswordHash != ""})
}
func (app *App) handleLogin(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Password string `json:"password"`
	}
	if decodeJSON(w, r, &input) != nil {
		return
	}
	app.authMu.Lock()
	defer app.authMu.Unlock()
	if time.Now().Before(app.nextLogin) {
		writeError(w, 429, "Повторите вход через несколько секунд")
		return
	}
	app.nextLogin = time.Now().Add(time.Second)
	s := app.settings()
	if app.needsSetup() || s.PasswordHash == "" || bcrypt.CompareHashAndPassword([]byte(s.PasswordHash), []byte(input.Password)) != nil {
		writeError(w, 401, "Неверный пароль")
		return
	}
	app.setSession(w)
	writeJSON(w, 200, map[string]bool{"ok": true})
}
func (app *App) handleLogout(w http.ResponseWriter, _ *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: "orchestrator_session", Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteStrictMode})
	writeJSON(w, 200, map[string]bool{"ok": true})
}
func (app *App) handleSettings(w http.ResponseWriter, _ *http.Request) {
	s := app.settings()
	writeJSON(w, 200, map[string]any{"base_url": s.BaseURL, "certbot_email": s.CertbotEmail, "duckdns_configured": s.DuckDNSToken != "", "password_enabled": s.PasswordHash != "", "base_url_locked": app.config.BaseURL != "", "certbot_email_locked": app.config.CertbotEmail != ""})
}
func (app *App) handleSaveSettings(w http.ResponseWriter, r *http.Request) {
	var input struct {
		BaseURL      string  `json:"base_url"`
		CertbotEmail string  `json:"certbot_email"`
		DuckDNSToken *string `json:"duckdns_token"`
		Password     string  `json:"password"`
		SetupToken   string  `json:"setup_token"`
	}
	if decodeJSON(w, r, &input) != nil {
		return
	}
	app.operationMu.Lock()
	defer app.operationMu.Unlock()
	setup := app.needsSetup()
	old := app.settings()
	// Recheck under the lock so racing first-run requests cannot replace credentials.
	if setup {
		if old.SetupToken == "" || subtle.ConstantTimeCompare([]byte(input.SetupToken), []byte(old.SetupToken)) != 1 {
			writeError(w, 403, "Введите код настройки из журнала запуска")
			return
		}
	} else if !app.authenticated(r) {
		writeError(w, 401, "Требуется вход")
		return
	}
	base, err := normalizeBaseURL(input.BaseURL)
	if app.config.BaseURL != "" {
		base = app.config.BaseURL
		err = nil
	}
	email := strings.TrimSpace(input.CertbotEmail)
	if app.config.CertbotEmail != "" {
		email = app.config.CertbotEmail
	}
	if err != nil || !validEmail(email) {
		writeError(w, 400, "Укажите URL панели и корректный email для сертификатов")
		return
	}
	hash := old.PasswordHash
	secret := old.SessionSecret
	if setup && input.Password == "" {
		writeError(w, 400, "Задайте пароль администратора")
		return
	}
	if input.Password != "" {
		if len(input.Password) < 12 || len(input.Password) > 72 {
			writeError(w, 400, "Пароль должен содержать от 12 до 72 байт")
			return
		}
		encoded, e := bcrypt.GenerateFromPassword([]byte(input.Password), bcrypt.DefaultCost)
		if e != nil {
			writeError(w, 500, "Не удалось сохранить пароль")
			return
		}
		hash = string(encoded)
		secret, err = randomToken()
		if err != nil {
			writeError(w, 500, "Не удалось создать сессию")
			return
		}
	}
	if input.DuckDNSToken != nil && strings.ContainsAny(*input.DuckDNSToken, "\r\n\x00") {
		writeError(w, 400, "Некорректный токен DuckDNS")
		return
	}
	err = app.store.Update(func(state *State) error {
		state.Settings.BaseURL = base
		state.Settings.CertbotEmail = email
		state.Settings.PasswordHash = hash
		state.Settings.SessionSecret = secret
		state.Settings.SetupToken = ""
		if input.DuckDNSToken != nil {
			state.Settings.DuckDNSToken = strings.TrimSpace(*input.DuckDNSToken)
		}
		return nil
	})
	if err != nil {
		writeError(w, 500, fmt.Sprintf("Не удалось сохранить настройки: %v", err))
		return
	}
	if hash != "" {
		app.setSession(w)
	}
	app.handleSettings(w, r)
}
