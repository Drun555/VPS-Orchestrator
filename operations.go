package main

import (
	"context"
	"fmt"
	"strings"
	"time"
)

type AddServerRequest struct {
	DisplayName  string `json:"display_name"`
	Address      string `json:"address"`
	PrivateKey   string `json:"private_key"`
	Passphrase   string `json:"passphrase"`
	PasswordAuth bool   `json:"password_auth"`
	Password     string `json:"password"`
	PublicKey    string `json:"public_key"`
	DuckDNSURL   string `json:"duckdns_url"`
	DuckDNSToken string `json:"duckdns_token"`
}

type UpdateServerRequest struct {
	DisplayName string `json:"display_name"`
}

type AddUserRequest struct {
	Email      string   `json:"email"`
	ServerMode string   `json:"server_mode"`
	ServerIDs  []string `json:"server_ids"`
}
type UpdateUserRequest struct {
	ServerMode string   `json:"server_mode"`
	ServerIDs  []string `json:"server_ids"`
}

func (app *App) provisionServer(ctx context.Context, request AddServerRequest, reporter *JobReporter) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()
	app.operationMu.Lock()
	defer app.operationMu.Unlock()

	host, port, normalizedAddress, err := parseSSHAddress(request.Address)
	if err != nil {
		return err
	}
	domain, err := normalizeDuckDNS(request.DuckDNSURL)
	if err != nil {
		return err
	}
	displayName, err := normalizeServerDisplayName(request.DisplayName)
	if err != nil {
		return err
	}
	if strings.TrimSpace(request.DuckDNSToken) == "" {
		return fmt.Errorf("DuckDNS token is required")
	}
	signer, err := parsePrivateSigner(request.PrivateKey, request.Passphrase)
	if err != nil {
		return err
	}
	publicKey := authorizedPublicKey(signer)
	if request.PasswordAuth {
		if request.Password == "" {
			return fmt.Errorf("SSH password is required in password mode")
		}
		if strings.TrimSpace(request.PublicKey) == "" {
			return fmt.Errorf("public key is required in password mode")
		}
		if err := validatePublicKeyMatches(signer, request.PublicKey); err != nil {
			return err
		}
		publicKey = strings.TrimSpace(request.PublicKey)
	}

	state := app.store.Snapshot()
	for _, existing := range state.Servers {
		if strings.EqualFold(existing.Address, normalizedAddress) || strings.EqualFold(existing.DuckDNSURL, domain) {
			return fmt.Errorf("server with this address or DuckDNS domain already exists")
		}
	}

	reporter.Log("Подключение к root@%s…", normalizedAddress)
	client, fingerprint, err := dialSSH(ctx, host, port, SSHCredential{
		PrivateKey: request.PrivateKey, Passphrase: request.Passphrase,
		Password: request.Password, UsePassword: request.PasswordAuth,
	}, "")
	if err != nil {
		return err
	}
	defer client.Close()
	reporter.Log("SSH host key принят по TOFU: %s", fingerprint)

	args := "--setup-ssh-key " + shellQuote(publicKey) +
		" --duckdns-token " + shellQuote(request.DuckDNSToken) +
		" --duckdns-url " + shellQuote(domain) +
		" --certbot-email " + shellQuote(app.config.CertbotEmail)
	reporter.Log("Установка встроенного комплекта VPS…")
	if err := runEmbeddedInstaller(client, "setup.sh", args, reporter); err != nil {
		return err
	}

	id, err := randomID()
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	server := Server{
		ID: id, DisplayName: displayName, Address: normalizedAddress, SSHHost: host, SSHPort: port,
		SSHPrivateKey: strings.TrimSpace(request.PrivateKey), SSHPassphrase: request.Passphrase,
		SSHHostFingerprint: fingerprint, PublicKey: publicKey,
		DuckDNSURL: domain, DuckDNSToken: request.DuckDNSToken,
		Status: "ready", CreatedAt: now, UpdatedAt: now,
	}
	if err := app.store.Update(func(state *State) error {
		for _, existing := range state.Servers {
			if strings.EqualFold(existing.Address, normalizedAddress) || strings.EqualFold(existing.DuckDNSURL, domain) {
				return fmt.Errorf("server with this address or DuckDNS domain already exists")
			}
		}
		state.Servers = append(state.Servers, server)
		return nil
	}); err != nil {
		return fmt.Errorf("setup завершился, но сервер не сохранён: %w. Выполните uninstall.sh вручную", err)
	}
	reporter.Log("Сервер сохранён. Синхронизация существующих пользователей…")

	app.reconcileServer(ctx, server, reporter)
	reporter.Log("VPS установлен: %s", domain)
	return nil
}

func (app *App) cleanupFailedServer(ctx context.Context, request AddServerRequest, reporter *JobReporter) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()
	app.operationMu.Lock()
	defer app.operationMu.Unlock()

	host, port, normalizedAddress, err := parseSSHAddress(request.Address)
	if err != nil {
		return err
	}
	domain, err := normalizeDuckDNS(request.DuckDNSURL)
	if err != nil {
		return err
	}

	credentials := []SSHCredential{{PrivateKey: request.PrivateKey, Passphrase: request.Passphrase}}
	if request.PasswordAuth {
		credentials = append(credentials, SSHCredential{
			PrivateKey: request.PrivateKey, Passphrase: request.Passphrase,
			Password: request.Password, UsePassword: true,
		})
	}

	var clientError error
	for index, credential := range credentials {
		mode := "SSH-ключу"
		if credential.UsePassword {
			mode = "паролю"
		}
		reporter.Log("Подключение к root@%s по %s для удаления…", normalizedAddress, mode)
		client, fingerprint, dialErr := dialSSH(ctx, host, port, credential, "")
		if dialErr != nil {
			clientError = dialErr
			if index+1 < len(credentials) {
				reporter.Warning("Подключение по ключу не удалось; пробуем исходный пароль.")
			}
			continue
		}
		reporter.Log("SSH host key принят по TOFU: %s", fingerprint)
		defer client.Close()
		if err := runEmbeddedInstaller(client, "uninstall.sh", "--yes --domain "+shellQuote(domain), reporter); err != nil {
			return err
		}
		reporter.Log("Следы неудачной установки удалены.")
		return nil
	}
	return fmt.Errorf("SSH-подключение для удаления не выполнено: %w", clientError)
}

func validateAssignment(state State, mode string, ids []string) (string, []string, error) {
	if mode == "" {
		mode = "all"
	}
	if mode != "all" && mode != "selected" {
		return "", nil, fmt.Errorf("server_mode must be all or selected")
	}
	selected := []string{}
	seen := map[string]bool{}
	for _, id := range ids {
		if server, _ := findServer(&state, id); server == nil {
			return "", nil, fmt.Errorf("unknown server ID %q", id)
		}
		if !seen[id] {
			selected = append(selected, id)
			seen[id] = true
		}
	}
	if mode == "all" {
		selected = []string{}
	}
	return mode, selected, nil
}

func (app *App) createUser(ctx context.Context, request AddUserRequest, reporter *JobReporter) error {
	app.operationMu.Lock()
	defer app.operationMu.Unlock()
	email := strings.TrimSpace(request.Email)
	if !validProfileName(email) {
		return fmt.Errorf("profile name must contain between 1 and 254 bytes")
	}
	state := app.store.Snapshot()
	mode, ids, err := validateAssignment(state, request.ServerMode, request.ServerIDs)
	if err != nil {
		return err
	}
	for _, user := range state.Users {
		if normalizeEmail(user.Email) == normalizeEmail(email) {
			return fmt.Errorf("user with this email already exists")
		}
	}
	id, err := randomID()
	if err != nil {
		return err
	}
	token, err := randomToken()
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	user := User{ID: id, Email: email, ServerMode: mode, ServerIDs: ids, SubscriptionToken: token, Links: map[string]UserLink{}, CreatedAt: now, UpdatedAt: now}
	if err = app.store.Update(func(state *State) error { state.Users = append(state.Users, user); return nil }); err != nil {
		return err
	}
	reporter.Log("Пользователь %s сохранён.", email)
	for _, server := range state.Servers {
		if userAllowsServer(user, server.ID) {
			app.reconcileServer(ctx, server, reporter)
		}
	}
	return nil
}

func (app *App) updateUser(ctx context.Context, userID string, request UpdateUserRequest, reporter *JobReporter) error {
	app.operationMu.Lock()
	defer app.operationMu.Unlock()
	state := app.store.Snapshot()
	previous, _ := findUser(&state, userID)
	if previous == nil {
		return fmt.Errorf("user not found")
	}
	mode, ids, err := validateAssignment(state, request.ServerMode, request.ServerIDs)
	if err != nil {
		return err
	}
	if err = app.store.Update(func(state *State) error {
		user, _ := findUser(state, userID)
		user.ServerMode = mode
		user.ServerIDs = ids
		user.UpdatedAt = time.Now().UTC()
		for id, link := range user.Links {
			if !userAllowsServer(*user, id) {
				link.Status = "revoking"
				link.LastError = "Ожидается отзыв доступа"
				user.Links[id] = link
			}
		}
		return nil
	}); err != nil {
		return err
	}
	current := app.store.Snapshot()
	user, _ := findUser(&current, userID)
	for _, server := range current.Servers {
		_, tracked := user.Links[server.ID]
		if userAllowsServer(*previous, server.ID) || userAllowsServer(*user, server.ID) || tracked {
			app.reconcileServer(ctx, server, reporter)
		}
	}
	return nil
}

func (app *App) retryUser(ctx context.Context, userID string, reporter *JobReporter) error {
	app.operationMu.Lock()
	defer app.operationMu.Unlock()
	state := app.store.Snapshot()
	user, _ := findUser(&state, userID)
	if user == nil {
		return fmt.Errorf("user not found")
	}
	for _, server := range state.Servers {
		_, tracked := user.Links[server.ID]
		if userAllowsServer(*user, server.ID) || tracked {
			app.reconcileServer(ctx, server, reporter)
		}
	}
	return nil
}

func (app *App) deleteUser(ctx context.Context, userID string, reporter *JobReporter) error {
	app.operationMu.Lock()
	defer app.operationMu.Unlock()
	if err := app.store.Update(func(state *State) error {
		_, i := findUser(state, userID)
		if i < 0 {
			return fmt.Errorf("user not found")
		}
		state.Users = append(state.Users[:i], state.Users[i+1:]...)
		return nil
	}); err != nil {
		return err
	}
	reporter.Log("Пользователь удалён; ссылка подписки уже недействительна.")
	for _, server := range app.store.Snapshot().Servers {
		app.reconcileServer(ctx, server, reporter)
	}
	return nil
}

func (app *App) deleteServer(ctx context.Context, serverID, mode string, reporter *JobReporter) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	app.operationMu.Lock()
	defer app.operationMu.Unlock()
	state := app.store.Snapshot()
	server, _ := findServer(&state, serverID)
	if server == nil {
		return fmt.Errorf("server not found")
	}
	if mode == "forget" {
		reporter.Log("Сервер удаляется только из Master.")
		return app.forgetServer(serverID)
	}
	if mode != "uninstall" {
		return fmt.Errorf("delete mode must be uninstall or forget")
	}
	reporter.Log("Подключение к %s для uninstall…", server.Address)
	client, _, err := dialSSH(ctx, server.SSHHost, server.SSHPort, SSHCredential{
		PrivateKey: server.SSHPrivateKey, Passphrase: server.SSHPassphrase,
	}, server.SSHHostFingerprint)
	if err != nil {
		_ = app.setServerStatus(server.ID, "error", err.Error())
		return fmt.Errorf("SSH uninstall не выполнен: %w. Выполните uninstall.sh вручную", err)
	}
	defer client.Close()
	if err := runEmbeddedInstaller(client, "uninstall.sh", "--yes --domain "+shellQuote(server.DuckDNSURL), reporter); err != nil {
		_ = app.setServerStatus(server.ID, "error", err.Error())
		return err
	}
	return app.forgetServer(serverID)
}

func (app *App) fullSync(ctx context.Context, reporter *JobReporter) error {
	app.operationMu.Lock()
	defer app.operationMu.Unlock()
	for _, server := range app.store.Snapshot().Servers {
		app.reconcileServer(ctx, server, reporter)
	}
	return nil
}

func (app *App) reconcileServer(ctx context.Context, server Server, reporter *JobReporter) {
	state := app.store.Snapshot()
	reporter.Log("%s: согласование пользователей по SSH…", server.DuckDNSURL)
	links, remoteErr := app.servers.Reconcile(ctx, server, state.Users, reporter)
	err := app.store.Update(func(state *State) error {
		target, _ := findServer(state, server.ID)
		if target == nil {
			return fmt.Errorf("server not found")
		}
		target.UpdatedAt = time.Now().UTC()
		target.Status = "ready"
		target.LastError = ""
		if remoteErr != nil {
			target.Status = "error"
			target.LastError = remoteErr.Error()
		}
		for i := range state.Users {
			user := &state.Users[i]
			if user.Links == nil {
				user.Links = map[string]UserLink{}
			}
			if remoteErr == nil {
				if link, ok := links[user.ID]; ok {
					user.Links[server.ID] = link
				} else {
					delete(user.Links, server.ID)
				}
			} else {
				link, exists := user.Links[server.ID]
				if userAllowsServer(*user, server.ID) {
					link.Status = "error"
				} else if exists {
					link.Status = "revoking"
				} else {
					continue
				}
				link.ServerID = server.ID
				link.LastError = remoteErr.Error()
				link.UpdatedAt = target.UpdatedAt
				user.Links[server.ID] = link
			}
			user.UpdatedAt = target.UpdatedAt
		}
		return nil
	})
	if remoteErr != nil {
		reporter.Warning("%s: %v", server.DuckDNSURL, remoteErr)
	}
	if err != nil {
		reporter.Warning("%s: состояние не сохранено: %v", server.DuckDNSURL, err)
	}
}

func (app *App) setUserLink(userID, serverID string, link UserLink) error {
	return app.store.Update(func(state *State) error {
		user, _ := findUser(state, userID)
		if user == nil {
			return fmt.Errorf("user not found")
		}
		if user.Links == nil {
			user.Links = make(map[string]UserLink)
		}
		user.Links[serverID] = link
		user.UpdatedAt = time.Now().UTC()
		return nil
	})
}

func (app *App) setServerStatus(serverID, status, lastError string) error {
	return app.store.Update(func(state *State) error {
		server, _ := findServer(state, serverID)
		if server == nil {
			return fmt.Errorf("server not found")
		}
		server.Status = status
		server.LastError = lastError
		server.UpdatedAt = time.Now().UTC()
		return nil
	})
}

func (app *App) forgetServer(serverID string) error {
	return app.store.Update(func(state *State) error {
		_, index := findServer(state, serverID)
		if index < 0 {
			return fmt.Errorf("server not found")
		}
		state.Servers = append(state.Servers[:index], state.Servers[index+1:]...)
		for userIndex := range state.Users {
			delete(state.Users[userIndex].Links, serverID)
			ids := state.Users[userIndex].ServerIDs[:0]
			for _, id := range state.Users[userIndex].ServerIDs {
				if id != serverID {
					ids = append(ids, id)
				}
			}
			state.Users[userIndex].ServerIDs = ids
			state.Users[userIndex].UpdatedAt = time.Now().UTC()
		}
		return nil
	})
}
