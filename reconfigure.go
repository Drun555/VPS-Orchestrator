package main

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"
)

func (app *App) handleReconfigureServer(w http.ResponseWriter, r *http.Request) {
	var input AddServerRequest
	if decodeJSON(w, r, &input) != nil {
		return
	}
	id := r.PathValue("id")
	state := app.store.Snapshot()
	if server, _ := findServer(&state, id); server == nil {
		writeError(w, http.StatusNotFound, "server not found")
		return
	}
	job, err := app.jobs.Start("server_reconfigure", []string{input.Password, input.PrivateKey, input.Passphrase, input.DuckDNSToken}, func(reporter *JobReporter) error {
		return app.reconfigureServer(context.Background(), id, input, reporter)
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"job_id": job.id})
}

func prepareReconfiguration(state State, id string, input AddServerRequest, sharedToken string) (Server, error) {
	current, _ := findServer(&state, id)
	if current == nil {
		return Server{}, errServerNotFound
	}
	server := *current
	var err error
	if server.SSHHost, server.SSHPort, server.Address, err = parseSSHAddress(input.Address); err != nil {
		return Server{}, err
	}
	if server.DuckDNSURL, err = normalizeDuckDNS(input.DuckDNSURL); err != nil {
		return Server{}, err
	}
	if server.DisplayName, err = normalizeServerDisplayName(input.DisplayName); err != nil {
		return Server{}, err
	}
	if server.SiteTemplate, err = validateSiteTemplate(input.SiteTemplate); err != nil {
		return Server{}, err
	}
	for _, other := range state.Servers {
		if other.ID != id && (strings.EqualFold(other.Address, server.Address) || strings.EqualFold(other.DuckDNSURL, server.DuckDNSURL)) {
			return Server{}, fmt.Errorf("SSH-адрес или DuckDNS уже используется другим сервером")
		}
	}
	if input.PasswordAuth && input.Password == "" {
		return Server{}, fmt.Errorf("Укажите новый root-пароль после Rebuild")
	}
	// Blank credentials reuse the stored identity without exposing it to the browser.
	if strings.TrimSpace(input.PrivateKey) != "" {
		server.SSHPrivateKey, server.SSHPassphrase = input.PrivateKey, input.Passphrase
	} else if server.SSHPrivateKey == "" && input.PasswordAuth {
		server.SSHPrivateKey, err = generateSSHKey()
		server.SSHPassphrase = ""
		if err != nil {
			return Server{}, err
		}
	}
	signer, err := parsePrivateSigner(server.SSHPrivateKey, server.SSHPassphrase)
	if err != nil {
		return Server{}, err
	}
	if input.PublicKey != "" {
		if err := validatePublicKeyMatches(signer, input.PublicKey); err != nil {
			return Server{}, err
		}
	}
	server.PublicKey = authorizedPublicKey(signer)
	if token := strings.TrimSpace(input.DuckDNSToken); token != "" {
		server.DuckDNSToken, server.SharedDuckDNS = token, false
	}
	if (server.SharedDuckDNS && sharedToken == "") || (!server.SharedDuckDNS && server.DuckDNSToken == "") {
		return Server{}, fmt.Errorf("Задайте токен DuckDNS в настройках")
	}
	server.SetupAttemptID, err = randomID()
	if err != nil {
		return Server{}, err
	}
	server.Private = input.Private
	server.SSHHostFingerprint = ""
	server.Provisioning, server.Status, server.LastError = true, "provisioning", ""
	server.UpdatedAt = time.Now().UTC()
	return server, nil
}

func (app *App) reconfigureServer(ctx context.Context, id string, input AddServerRequest, reporter *JobReporter) error {
	app.operationMu.Lock()
	defer app.operationMu.Unlock()
	server, err := prepareReconfiguration(app.store.Snapshot(), id, input, app.settings().DuckDNSToken)
	if err != nil {
		return err
	}
	if err := app.store.Update(func(state *State) error {
		target, _ := findServer(state, id)
		if target == nil {
			return errServerNotFound
		}
		*target = server
		for i := range state.Users {
			if link, ok := state.Users[i].Links[id]; ok {
				link.URI, link.Status, link.LastError = "", "pending", "Ожидается повторная настройка сервера"
				link.UpdatedAt = server.UpdatedAt
				state.Users[i].Links[id] = link
			}
		}
		return nil
	}); err != nil {
		return err
	}
	reporter.Log("Повторная настройка после Rebuild: новый SSH host key будет принят, все этапы установки будут выполнены заново.")
	password := ""
	if input.PasswordAuth {
		password = input.Password
	}
	return app.continueProvision(ctx, server, password, reporter)
}
