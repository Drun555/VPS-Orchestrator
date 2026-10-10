package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"fmt"
	"golang.org/x/crypto/ssh"
	"net/http"
	"strings"
	"time"
)

func generateSSHKey() (string, error) {
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return "", err
	}
	block, err := ssh.MarshalPrivateKey(key, "vps-orchestrator")
	if err != nil {
		return "", err
	}
	return string(pem.EncodeToMemory(block)), nil
}
func (app *App) provisionServer(ctx context.Context, request AddServerRequest, reporter *JobReporter) error {
	app.operationMu.Lock()
	defer app.operationMu.Unlock()
	host, port, address, err := parseSSHAddress(request.Address)
	if err != nil {
		return err
	}
	domain, err := normalizeDuckDNS(request.DuckDNSURL)
	if err != nil {
		return err
	}
	name, err := normalizeServerDisplayName(request.DisplayName)
	if err != nil {
		return err
	}
	for _, s := range app.store.Snapshot().Servers {
		if strings.EqualFold(s.Address, address) || strings.EqualFold(s.DuckDNSURL, domain) {
			return fmt.Errorf("Сервер уже сохранён. Используйте «Продолжить установку» в его карточке")
		}
	}
	shared := strings.TrimSpace(request.DuckDNSToken) == ""
	if shared {
		request.DuckDNSToken = app.settings().DuckDNSToken
	}
	if request.DuckDNSToken == "" {
		return fmt.Errorf("Задайте общий токен DuckDNS в настройках или токен этого сервера")
	}
	if request.PasswordAuth && request.Password == "" {
		return fmt.Errorf("Укажите root-пароль")
	}
	if request.PasswordAuth && strings.TrimSpace(request.PrivateKey) == "" {
		request.PrivateKey, err = generateSSHKey()
		if err != nil {
			return err
		}
		request.Passphrase = ""
	}
	signer, err := parsePrivateSigner(request.PrivateKey, request.Passphrase)
	if err != nil {
		return err
	}
	if request.PublicKey != "" {
		if err = validatePublicKeyMatches(signer, request.PublicKey); err != nil {
			return err
		}
	}
	id, err := randomID()
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	server := Server{ID: id, DisplayName: name, Address: address, SSHHost: host, SSHPort: port, SSHPrivateKey: request.PrivateKey, SSHPassphrase: request.Passphrase, PublicKey: authorizedPublicKey(signer), DuckDNSURL: domain, DuckDNSToken: request.DuckDNSToken, SharedDuckDNS: shared, Provisioning: true, Status: "provisioning", CreatedAt: now, UpdatedAt: now}
	// Save the generated identity before any remote changes. Never persist the root password.
	if err = app.store.Update(func(s *State) error { s.Servers = append(s.Servers, server); return nil }); err != nil {
		return err
	}
	return app.continueProvision(ctx, server, request.Password, reporter)
}
func (app *App) handleResumeServer(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Password     string  `json:"password"`
		DuckDNSToken *string `json:"duckdns_token"`
	}
	if decodeJSON(w, r, &input) != nil {
		return
	}
	id := r.PathValue("id")
	state := app.store.Snapshot()
	server, _ := findServer(&state, id)
	if server == nil {
		writeError(w, 404, "server not found")
		return
	}
	if !server.Provisioning {
		writeError(w, 409, "Установка уже завершена")
		return
	}
	redactions := []string{input.Password}
	if input.DuckDNSToken != nil {
		redactions = append(redactions, *input.DuckDNSToken)
	}
	job, err := app.jobs.Start("server_resume", redactions, func(reporter *JobReporter) error {
		app.operationMu.Lock()
		defer app.operationMu.Unlock()
		state := app.store.Snapshot()
		server, _ := findServer(&state, id)
		if server == nil {
			return fmt.Errorf("server not found")
		}
		if !server.Provisioning {
			return fmt.Errorf("Установка уже завершена")
		}
		if input.DuckDNSToken != nil {
			server.DuckDNSToken = strings.TrimSpace(*input.DuckDNSToken)
			server.SharedDuckDNS = server.DuckDNSToken == ""
			if err := app.store.Update(func(state *State) error {
				target, _ := findServer(state, id)
				target.DuckDNSToken = server.DuckDNSToken
				target.SharedDuckDNS = server.SharedDuckDNS
				return nil
			}); err != nil {
				return err
			}
		}
		return app.continueProvision(context.Background(), *server, input.Password, reporter)
	})
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	writeJSON(w, 202, map[string]string{"job_id": job.id})
}
func (app *App) continueProvision(ctx context.Context, server Server, password string, reporter *JobReporter) (result error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()
	if server.SharedDuckDNS {
		server.DuckDNSToken = app.settings().DuckDNSToken
	}
	reporter.Redact(server.SSHPrivateKey, server.SSHPassphrase, server.DuckDNSToken, password)
	defer func() {
		if result != nil {
			if err := app.setServerStatus(server.ID, "setup_failed", reporter.SafeError(result)); err != nil {
				reporter.Warning("Не удалось сохранить ошибку установки: %v", err)
			}
		}
	}()
	if server.DuckDNSToken == "" {
		return fmt.Errorf("Задайте общий токен DuckDNS в настройках")
	}
	if err := app.setServerStatus(server.ID, "provisioning", ""); err != nil {
		return err
	}
	reporter.Log("Подключение к root@%s…", server.Address)
	credential := SSHCredential{PrivateKey: server.SSHPrivateKey, Passphrase: server.SSHPassphrase}
	client, fingerprint, err := dialSSH(ctx, server.SSHHost, server.SSHPort, credential, server.SSHHostFingerprint)
	if err != nil && password != "" {
		// Never fall back past a host-key mismatch; the same pin applies to both attempts.
		if server.SSHHostFingerprint == "" {
			server.SSHHostFingerprint = fingerprint
		}
		client, fingerprint, err = dialSSH(ctx, server.SSHHost, server.SSHPort, SSHCredential{UsePassword: true, Password: password}, server.SSHHostFingerprint)
	}
	if err != nil {
		return fmt.Errorf("SSH: %w. Если ключ ещё не установлен, укажите root-пароль при продолжении", err)
	}
	defer client.Close()
	server.SSHHostFingerprint = fingerprint
	if err = app.store.Update(func(state *State) error {
		s, _ := findServer(state, server.ID)
		if s == nil {
			return fmt.Errorf("server not found")
		}
		s.SSHHostFingerprint = fingerprint
		s.DuckDNSToken = server.DuckDNSToken
		return nil
	}); err != nil {
		return err
	}
	reporter.Log("SSH fingerprint: %s. Установка и проверка ключа…", fingerprint)
	// A separate append preserves the owner's keys. The new connection is verified before hardening.
	command := "set -eu; umask 077; mkdir -p /root/.ssh; chmod 700 /root/.ssh; touch /root/.ssh/authorized_keys; chmod 600 /root/.ssh/authorized_keys; key=" + shellQuote(server.PublicKey) + "; grep -qxF -- \"$key\" /root/.ssh/authorized_keys || printf '\\n%s\\n' \"$key\" >> /root/.ssh/authorized_keys"
	if err = runSSHCommand(client, command, reporter); err != nil {
		return fmt.Errorf("Установка SSH-ключа: %w", err)
	}
	verified, _, err := dialSSH(ctx, server.SSHHost, server.SSHPort, credential, fingerprint)
	if err != nil {
		return fmt.Errorf("Проверка входа по сохранённому ключу: %w", err)
	}
	defer verified.Close()
	_ = client.Close()
	reporter.Log("Вход по сохранённому ключу проверен. Продолжение установки…")
	args := "--installation-id " + shellQuote(server.ID) + " --setup-ssh-key " + shellQuote(server.PublicKey) + " --duckdns-token " + shellQuote(server.DuckDNSToken) + " --duckdns-url " + shellQuote(server.DuckDNSURL) + " --certbot-email " + shellQuote(app.settings().CertbotEmail)
	if err = runEmbeddedInstaller(verified, "setup.sh", args, reporter); err != nil {
		return err
	}
	if err = app.store.Update(func(state *State) error {
		s, _ := findServer(state, server.ID)
		s.Provisioning = false
		s.Status = "ready"
		s.LastError = ""
		s.UpdatedAt = time.Now().UTC()
		return nil
	}); err != nil {
		return err
	}
	server.Provisioning = false
	app.reconcileServer(ctx, server, reporter)
	reporter.Log("VPS установлен: %s", server.DuckDNSURL)
	return nil
}
