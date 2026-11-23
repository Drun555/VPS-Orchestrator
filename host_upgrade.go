package main

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"
)

type packageUpgradeManager interface {
	UpgradePackages(context.Context, Server, bool, *JobReporter) error
}

// Use apt-get's scripting interface and keep local configuration.
const hostUpgradeScript = `set -eu
export DEBIAN_FRONTEND=noninteractive
export NEEDRESTART_MODE=a
export UCF_FORCE_CONFFOLD=1
command -v apt-get >/dev/null || { echo 'APT не найден на хосте' >&2; exit 1; }
apt-get -o DPkg::Lock::Timeout=300 -o APT::Update::Error-Mode=any update
apt-get -o DPkg::Lock::Timeout=300 -o Dpkg::Options::=--force-confold --with-new-pkgs -y upgrade
`

const hostBootIDCommand = "cat /proc/sys/kernel/random/boot_id"
const hostRebootCommand = "systemd-run --on-active=5s --timer-property=AccuracySec=1s /usr/bin/systemctl reboot"
const hostPostRebootCommand = "set -eu; systemctl is-active --quiet xray; systemctl is-active --quiet nginx; " + hostBootIDCommand

func (SSHServerManager) UpgradePackages(ctx context.Context, server Server, waitForReboot bool, reporter *JobReporter) error {
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
		return fmt.Errorf("обновление пакетов завершилось ошибкой: %w", err)
	}
	remote := sshRemote{client: client}
	bootID := ""
	if waitForReboot {
		output, err := remote.Run(hostBootIDCommand, nil)
		if err != nil {
			return fmt.Errorf("чтение идентификатора загрузки: %w", err)
		}
		bootID = strings.TrimSpace(string(output))
		if bootID == "" {
			return fmt.Errorf("пустой идентификатор загрузки хоста")
		}
	}
	// A systemd timer acknowledges the request before reboot closes SSH.
	if _, err := remote.Run(hostRebootCommand, nil); err != nil {
		return fmt.Errorf("запуск перезагрузки: %w", err)
	}
	_ = client.Close()
	if !waitForReboot {
		return nil
	}
	reporter.Log("%s: перезагрузка запланирована; ожидаем SSH, Xray и Nginx…", server.Address)
	rebootCtx, rebootCancel := context.WithTimeout(ctx, 5*time.Minute)
	defer rebootCancel()
	return waitForHostReboot(rebootCtx, bootID, 5*time.Second, func(ctx context.Context) (string, error) {
		probeCtx, probeCancel := context.WithTimeout(ctx, 15*time.Second)
		defer probeCancel()
		client, _, err := dialSSH(probeCtx, server.SSHHost, server.SSHPort, SSHCredential{
			PrivateKey: server.SSHPrivateKey, Passphrase: server.SSHPassphrase,
		}, server.SSHHostFingerprint)
		if err != nil {
			return "", err
		}
		defer client.Close()
		output, err := (sshRemote{client: client}).Run(hostPostRebootCommand, nil)
		return strings.TrimSpace(string(output)), err
	})
}

func waitForHostReboot(ctx context.Context, previousBootID string, interval time.Duration, probe func(context.Context) (string, error)) error {
	lastError := "хост ещё не перезагрузился"
	for {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("ожидание перезагрузки и запуска Xray/Nginx: %w (%s)", err, lastError)
		}
		bootID, err := probe(ctx)
		if err == nil && bootID != "" && bootID != previousBootID {
			return nil
		}
		if err != nil {
			lastError = err.Error()
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
		case <-timer.C:
		}
	}
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
		if err := manager.UpgradePackages(ctx, server, id != "", reporter); err != nil {
			failed++
			reporter.Warning("%s: %v", server.Address, err)
			continue
		}
		updated++
		if id == "" {
			reporter.Log("%s: пакеты обновлены, перезагрузка запущена; переходим к следующему хосту.", server.Address)
		} else {
			reporter.Log("%s: пакеты обновлены, хост перезагружен, Xray и Nginx работают.", server.Address)
		}
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
