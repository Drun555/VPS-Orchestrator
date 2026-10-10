package main

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

var errServerNotFound = errors.New("server not found")

//go:embed web/index.html web/assets/*
var embeddedWeb embed.FS

type App struct {
	config      Config
	store       *Store
	jobs        *JobManager
	servers     ServerManager
	operationMu sync.Mutex
	authMu      sync.Mutex
	nextLogin   time.Time
}

func NewApp(config Config, store *Store) *App {
	return &App{config: config, store: store, jobs: NewJobManager(), servers: SSHServerManager{}}
}

func (app *App) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", app.handleIndex)
	mux.HandleFunc("GET /assets/{name}", app.handleAsset)
	mux.HandleFunc("GET /api/session", app.handleSession)
	mux.HandleFunc("POST /api/login", app.handleLogin)
	mux.HandleFunc("POST /api/logout", app.handleLogout)
	mux.HandleFunc("GET /api/settings", app.handleSettings)
	mux.HandleFunc("PUT /api/settings", app.handleSaveSettings)
	mux.HandleFunc("POST /api/servers/{id}/resume", app.handleResumeServer)
	mux.HandleFunc("GET /api/overview", app.handleOverview)
	mux.HandleFunc("POST /api/servers", app.handleAddServer)
	mux.HandleFunc("POST /api/servers/cleanup", app.handleCleanupServer)
	mux.HandleFunc("PATCH /api/servers/{id}", app.handleUpdateServer)
	mux.HandleFunc("DELETE /api/servers/{id}", app.handleDeleteServer)
	mux.HandleFunc("POST /api/users", app.handleAddUser)
	mux.HandleFunc("PATCH /api/users/{id}", app.handleUpdateUser)
	mux.HandleFunc("DELETE /api/users/{id}", app.handleDeleteUser)
	mux.HandleFunc("POST /api/users/{id}/retry", app.handleRetryUser)
	mux.HandleFunc("POST /api/sync", app.handleFullSync)
	mux.HandleFunc("GET /api/jobs/{id}", app.handleJob)
	mux.HandleFunc("GET /api/jobs/{id}/events", app.handleJobEvents)
	mux.HandleFunc("GET /subscribe/{token}", app.handleSubscription)
	mux.HandleFunc("GET /subscribe/{token}/proxies", app.handleProxyProvider)
	mux.HandleFunc("GET /mihomo/direct-rules", app.handleDirectRules)
	return securityHeaders(app.authorize(mux))
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("X-Content-Type-Options", "nosniff")
		response.Header().Set("X-Frame-Options", "DENY")
		response.Header().Set("Referrer-Policy", "no-referrer")
		response.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; connect-src 'self'; img-src 'self' data:; base-uri 'none'; frame-ancestors 'none'")
		next.ServeHTTP(response, request)
	})
}

func (app *App) handleIndex(response http.ResponseWriter, _ *http.Request) {
	data, err := embeddedWeb.ReadFile("web/index.html")
	if err != nil {
		http.Error(response, "UI unavailable", http.StatusInternalServerError)
		return
	}
	response.Header().Set("Content-Type", "text/html; charset=utf-8")
	response.Header().Set("Cache-Control", "no-cache")
	_, _ = response.Write(data)
}

func (app *App) handleAsset(response http.ResponseWriter, request *http.Request) {
	name := request.PathValue("name")
	if name == "" || strings.ContainsAny(name, `/\\`) {
		http.NotFound(response, request)
		return
	}
	data, err := embeddedWeb.ReadFile("web/assets/" + name)
	if err != nil {
		http.NotFound(response, request)
		return
	}
	if strings.HasSuffix(name, ".css") {
		response.Header().Set("Content-Type", "text/css; charset=utf-8")
	} else if strings.HasSuffix(name, ".js") {
		response.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	} else if strings.HasSuffix(name, ".svg") {
		response.Header().Set("Content-Type", "image/svg+xml")
	}
	response.Header().Set("Cache-Control", "public, max-age=300")
	_, _ = response.Write(data)
}

func (app *App) handleOverview(response http.ResponseWriter, _ *http.Request) {
	state := app.store.Snapshot()
	servers := make([]ServerView, 0, len(state.Servers))
	for _, server := range state.Servers {
		servers = append(servers, serverView(server))
	}
	users := make([]UserView, 0, len(state.Users))
	for _, user := range state.Users {
		users = append(users, userView(user, app.settings().BaseURL))
	}
	writeJSON(response, http.StatusOK, map[string]any{"servers": servers, "users": users, "base_url": app.settings().BaseURL})
}

func (app *App) handleAddServer(response http.ResponseWriter, request *http.Request) {
	var input AddServerRequest
	if err := decodeJSON(response, request, &input); err != nil {
		return
	}
	if _, err := validateSiteTemplate(input.SiteTemplate); err != nil {
		writeError(response, http.StatusBadRequest, err.Error())
		return
	}

	job, err := app.jobs.Start("server_add", []string{input.PrivateKey, input.Passphrase, input.Password, input.DuckDNSToken}, func(reporter *JobReporter) error {
		return app.provisionServer(context.Background(), input, reporter)
	})
	if err != nil {
		writeError(response, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(response, http.StatusAccepted, map[string]string{"job_id": job.id})
}

func (app *App) handleCleanupServer(response http.ResponseWriter, request *http.Request) {
	var input AddServerRequest
	if err := decodeJSON(response, request, &input); err != nil {
		return
	}
	redactions := []string{input.PrivateKey, input.Passphrase, input.Password, input.DuckDNSToken}
	job, err := app.jobs.Start("server_cleanup", redactions, func(reporter *JobReporter) error {
		return app.cleanupFailedServer(context.Background(), input, reporter)
	})
	if err != nil {
		writeError(response, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(response, http.StatusAccepted, map[string]string{"job_id": job.id})
}

func (app *App) handleUpdateServer(response http.ResponseWriter, request *http.Request) {
	var input UpdateServerRequest
	if err := decodeJSON(response, request, &input); err != nil {
		return
	}
	displayName, err := normalizeServerDisplayName(input.DisplayName)
	if err != nil {
		writeError(response, http.StatusBadRequest, err.Error())
		return
	}
	serverID := request.PathValue("id")
	var updated Server
	err = app.store.Update(func(state *State) error {
		server, _ := findServer(state, serverID)
		if server == nil {
			return errServerNotFound
		}
		server.DisplayName = displayName
		server.UpdatedAt = time.Now().UTC()
		updated = *server
		return nil
	})
	if errors.Is(err, errServerNotFound) {
		writeError(response, http.StatusNotFound, "server not found")
		return
	}
	if err != nil {
		writeError(response, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(response, http.StatusOK, serverView(updated))
}

func (app *App) handleDeleteServer(response http.ResponseWriter, request *http.Request) {
	var input struct {
		Mode string `json:"mode"`
	}
	if err := decodeJSON(response, request, &input); err != nil {
		return
	}
	serverID := request.PathValue("id")
	state := app.store.Snapshot()
	server, _ := findServer(&state, serverID)
	if server == nil {
		writeError(response, http.StatusNotFound, "server not found")
		return
	}
	job, err := app.jobs.Start("server_delete", []string{server.SSHPrivateKey, server.SSHPassphrase, server.DuckDNSToken}, func(reporter *JobReporter) error {
		return app.deleteServer(context.Background(), serverID, input.Mode, reporter)
	})
	if err != nil {
		writeError(response, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(response, http.StatusAccepted, map[string]string{"job_id": job.id})
}

func (app *App) handleAddUser(response http.ResponseWriter, request *http.Request) {
	var input AddUserRequest
	if err := decodeJSON(response, request, &input); err != nil {
		return
	}
	if _, _, err := validateAssignment(app.store.Snapshot(), input.ServerMode, input.ServerIDs); err != nil {
		writeError(response, http.StatusBadRequest, err.Error())
		return
	}
	job, err := app.jobs.Start("user_add", nil, func(reporter *JobReporter) error {
		return app.createUser(context.Background(), input, reporter)
	})
	if err != nil {
		writeError(response, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(response, http.StatusAccepted, map[string]string{"job_id": job.id})
}

func (app *App) handleUpdateUser(response http.ResponseWriter, request *http.Request) {
	var input UpdateUserRequest
	if err := decodeJSON(response, request, &input); err != nil {
		return
	}
	state := app.store.Snapshot()
	id := request.PathValue("id")
	if user, _ := findUser(&state, id); user == nil {
		writeError(response, http.StatusNotFound, "user not found")
		return
	}
	if input.ServerMode == "" {
		writeError(response, http.StatusBadRequest, "server_mode is required")
		return
	}
	if _, _, err := validateAssignment(state, input.ServerMode, input.ServerIDs); err != nil {
		writeError(response, http.StatusBadRequest, err.Error())
		return
	}
	job, err := app.jobs.Start("user_update", nil, func(reporter *JobReporter) error { return app.updateUser(context.Background(), id, input, reporter) })
	if err != nil {
		writeError(response, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(response, http.StatusAccepted, map[string]string{"job_id": job.id})
}

func (app *App) handleDeleteUser(response http.ResponseWriter, request *http.Request) {
	userID := request.PathValue("id")
	state := app.store.Snapshot()
	user, _ := findUser(&state, userID)
	if user == nil {
		writeError(response, http.StatusNotFound, "user not found")
		return
	}
	job, err := app.jobs.Start("user_delete", nil, func(reporter *JobReporter) error {
		return app.deleteUser(context.Background(), userID, reporter)
	})
	if err != nil {
		writeError(response, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(response, http.StatusAccepted, map[string]string{"job_id": job.id})
}

func (app *App) handleRetryUser(response http.ResponseWriter, request *http.Request) {
	userID := request.PathValue("id")
	job, err := app.jobs.Start("user_retry", nil, func(reporter *JobReporter) error {
		return app.retryUser(context.Background(), userID, reporter)
	})
	if err != nil {
		writeError(response, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(response, http.StatusAccepted, map[string]string{"job_id": job.id})
}

func (app *App) handleFullSync(response http.ResponseWriter, _ *http.Request) {
	job, err := app.jobs.Start("full_sync", nil, func(reporter *JobReporter) error {
		return app.fullSync(context.Background(), reporter)
	})
	if err != nil {
		writeError(response, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(response, http.StatusAccepted, map[string]string{"job_id": job.id})
}

func (app *App) handleJob(response http.ResponseWriter, request *http.Request) {
	job, exists := app.jobs.Get(request.PathValue("id"))
	if !exists {
		writeError(response, http.StatusNotFound, "job not found")
		return
	}
	writeJSON(response, http.StatusOK, job.Snapshot())
}

func (app *App) handleJobEvents(response http.ResponseWriter, request *http.Request) {
	job, exists := app.jobs.Get(request.PathValue("id"))
	if !exists {
		writeError(response, http.StatusNotFound, "job not found")
		return
	}
	flusher, ok := response.(http.Flusher)
	if !ok {
		writeError(response, http.StatusInternalServerError, "streaming unavailable")
		return
	}
	after, _ := strconv.ParseInt(request.URL.Query().Get("after"), 10, 64)
	past, events, cancel := job.Subscribe(after)
	defer cancel()
	response.Header().Set("Content-Type", "text/event-stream")
	response.Header().Set("Cache-Control", "no-cache, no-store")
	response.Header().Set("Connection", "keep-alive")
	for _, event := range past {
		writeSSE(response, event)
	}
	flusher.Flush()
	keepalive := time.NewTicker(15 * time.Second)
	defer keepalive.Stop()
	for {
		select {
		case event, open := <-events:
			if !open {
				return
			}
			writeSSE(response, event)
			flusher.Flush()
		case <-keepalive.C:
			_, _ = io.WriteString(response, ": keepalive\n\n")
			flusher.Flush()
		case <-request.Context().Done():
			return
		}
	}
}

func (app *App) handleSubscription(response http.ResponseWriter, request *http.Request) {
	state := app.store.Snapshot()
	user := userBySubscriptionToken(&state, request.PathValue("token"))
	if user == nil {
		http.NotFound(response, request)
		return
	}
	response.Header().Set("Content-Type", "text/yaml; charset=utf-8")
	response.Header().Set("Cache-Control", "no-store")
	response.Header().Set("profile-update-interval", "6")
	_, _ = response.Write(buildMihomoConfig(app.settings().BaseURL, user.SubscriptionToken))
}

func (app *App) handleProxyProvider(response http.ResponseWriter, request *http.Request) {
	state := app.store.Snapshot()
	user := userBySubscriptionToken(&state, request.PathValue("token"))
	if user == nil {
		http.NotFound(response, request)
		return
	}
	response.Header().Set("Content-Type", "text/yaml; charset=utf-8")
	response.Header().Set("Cache-Control", "no-store")
	_, _ = response.Write(buildMihomoProvider(state, user))
}

func (app *App) handleDirectRules(response http.ResponseWriter, _ *http.Request) {
	response.Header().Set("Content-Type", "text/yaml; charset=utf-8")
	response.Header().Set("Cache-Control", "no-store")
	_, _ = response.Write(buildDirectRules())
}

func userBySubscriptionToken(state *State, token string) *User {
	for index := range state.Users {
		if state.Users[index].SubscriptionToken == token {
			return &state.Users[index]
		}
	}
	return nil
}

func writeSSE(response http.ResponseWriter, event JobEvent) {
	_, _ = fmt.Fprintf(response, "id: %d\ndata: %s\n\n", event.Sequence, marshalSSE(event))
}

func decodeJSON(response http.ResponseWriter, request *http.Request, target any) error {
	request.Body = http.MaxBytesReader(response, request.Body, 2*1024*1024)
	decoder := json.NewDecoder(request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		writeError(response, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		writeError(response, http.StatusBadRequest, "request body must contain one JSON object")
		return fmt.Errorf("multiple JSON values")
	}
	return nil
}

func writeJSON(response http.ResponseWriter, status int, value any) {
	response.Header().Set("Content-Type", "application/json; charset=utf-8")
	response.Header().Set("Cache-Control", "no-store")
	response.WriteHeader(status)
	if err := json.NewEncoder(response).Encode(value); err != nil {
		log.Printf("encode HTTP response: %v", err)
	}
}

func writeError(response http.ResponseWriter, status int, message string) {
	writeJSON(response, status, map[string]string{"error": message})
}
