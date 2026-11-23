package main

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"
)

type packageUpgradeManager interface {
	UpgradePackages(context.Context, Server, *JobReporter) error
}

// Use apt-get's scripting interface, keep local configuration and never reboot.
const hostUpgradeScript = `set -eu
export DEBIAN_FRONTEND=noninteractive
export NEEDRESTART_MODE=a
export UCF_FORCE_CONFFOLD=1
command -v apt-get >/dev/null || { echo 'APT не найден на хосте' >&2; exit 1; }
apt-get -o DPkg::Lock::Timeout=300 -o APT::Update::Error-Mode=any update
apt-get -o DPkg::Lock::Timeout=300 -o Dpkg::Options::=--force-confold --with-new-pkgs -y upgrade
systemctl is-active --quiet xray
systemctl is-active --quiet nginx
`

func (SSHServerManager) UpgradePackages(ctx context.Context, server Server, reporter *JobReporter) error {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Minute)
	defer cancel()
	client, _, err := dialSSH(ctx, server.SSHHost, server.SSHPort, SSHCredential{
		PrivateKey: server.SSHPrivateKey, Passphrase: server.SSHPassphrase,
	}, server.SSHHostFingerprint)
	if err != nil {
		return err
	}
	defer client.Close()
	if err := runSSHCommandInput(client, "bash -s", []byte(hostUpgradeScript), reporter); err != nil {
		return fmt.Errorf("обновление пакетов или проверка Xray/Nginx завершились ошибкой: %w", err)
	}
	output, err := (sshRemote{client: client}).Run("if [ -f /var/run/reboot-required ]; then printf required; fi", nil)
	if err != nil {
		return fmt.Errorf("проверка необходимости перезагрузки: %w", err)
	}
	if strings.TrimSpace(string(output)) == "required" {
		reporter.Warning("%s: требуется перезагрузка хоста. Выполните её отдельно в удобное время.", server.Address)
	}
	return nil
}

func (app *App) handleUpgradePackages(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id") // Empty ID selects all hosts.
	state := app.store.Snapshot()
	if id != "" {
		server, _ := findServer(&state, id)
		if server == nil {
			writeError(w, http.StatusNotFound, "server not found")
			return
		}
		if server.Provisioning {
			writeError(w, http.StatusConflict, "Сначала завершите установку сервера")
			return
		}
	}
	job, err := app.jobs.Start("host_upgrade", nil, func(reporter *JobReporter) error {
		return app.upgradePackages(context.Background(), id, reporter)
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"job_id": job.id})
}

func (app *App) upgradePackages(ctx context.Context, id string, reporter *JobReporter) error {
	app.operationMu.Lock()
	defer app.operationMu.Unlock()
	manager, ok := app.servers.(packageUpgradeManager)
	if !ok {
		return fmt.Errorf("Менеджер сервера не поддерживает обновление пакетов")
	}
	servers := app.store.Snapshot().Servers
	if id != "" {
		state := State{Servers: servers}
		server, _ := findServer(&state, id)
		if server == nil {
			return errServerNotFound
		}
		servers = []Server{*server}
	}
	// Register all secrets before streaming any host's package-manager output.
	for _, server := range servers {
		reporter.Redact(server.SSHPrivateKey, server.SSHPassphrase, server.DuckDNSToken)
	}
	updated, failed := 0, 0
	for _, server := range servers {
		if err := ctx.Err(); err != nil {
			return err
		}
		if server.Provisioning {
			if id != "" {
				return fmt.Errorf("Сначала завершите установку сервера")
			}
			reporter.Warning("%s: пропущен, установка не завершена.", server.Address)
			continue
		}
		reporter.Log("%s: apt update / upgrade…", server.Address)
		if err := manager.UpgradePackages(ctx, server, reporter); err != nil {
			failed++
			reporter.Warning("%s: %v", server.Address, err)
			continue
		}
		updated++
		reporter.Log("%s: пакеты обновлены, Xray и Nginx работают.", server.Address)
	}
	reporter.Log("Обновлено хостов: %d; ошибок: %d.", updated, failed)
	if failed > 0 {
		return fmt.Errorf("Не удалось обновить хостов: %d. Подробности в журнале", failed)
	}
	if updated == 0 {
		return fmt.Errorf("Нет хостов с завершённой установкой для обновления")
	}
	return nil
}
