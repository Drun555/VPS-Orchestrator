package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

const siteTransactionPath = runtimePath + "/site-transaction"
const siteIndexPath = "/opt/vps-reality/index.html"

// All configuration writers use clients.lock, including the legacy CGI.
func withServerRemote(ctx context.Context, server Server, work func(remoteCommand) error) error {
	client, _, err := dialSSH(ctx, server.SSHHost, server.SSHPort, SSHCredential{PrivateKey: server.SSHPrivateKey, Passphrase: server.SSHPassphrase}, server.SSHHostFingerprint)
	if err != nil {
		return err
	}
	defer client.Close()
	lock, err := client.NewSession()
	if err != nil {
		return err
	}
	defer lock.Close()
	input, err := lock.StdinPipe()
	if err != nil {
		return err
	}
	defer input.Close()
	output, err := lock.StdoutPipe()
	if err != nil {
		return err
	}
	if err = lock.Start("flock -w 30 " + runtimePath + "/clients.lock sh -c 'echo locked; cat >/dev/null'"); err != nil {
		return err
	}
	line, err := bufio.NewReader(output).ReadString('\n')
	if err != nil || strings.TrimSpace(line) != "locked" {
		return fmt.Errorf("cannot acquire remote configuration lock")
	}
	return work(sshRemote{client})
}

func (manager SSHServerManager) UpdateSite(ctx context.Context, current, desired Server, email string, commit func() error, reporter *JobReporter) error {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()
	return withServerRemote(ctx, current, func(r remoteCommand) error {
		return updateSiteRemote(r, current, desired, email, commit, reporter, func() {
			if manager.BeforeChange != nil {
				manager.BeforeChange(current, r)
			}
		})
	})
}

// If the master saved the new settings, finish the commit. Otherwise restore the
// previous configuration. This also covers process termination or a lost SSH ACK.
func siteRecoveryCommand(server Server) string {
	return `set -eu
 t=` + shellQuote(siteTransactionPath) + `
 if [ -f "$t/pending" ]; then
  if [ "$(cat "$t/target-domain")" = ` + shellQuote(server.DuckDNSURL) + ` ] && [ "$(cat "$t/target-template")" = ` + shellQuote(defaultSiteTemplate(server.SiteTemplate)) + ` ]; then
   rm -f "$t/pending"
  else
   cp -p "$t/index" ` + siteIndexPath + `
   if [ -f "$t/domain-change" ]; then
    cp -p "$t/xray" ` + xrayConfigPath + `
    cp -p "$t/nginx" ` + nginxSitePath + `
    cp -p "$t/domain" ` + runtimePath + `/domain
    nginx -t >/dev/null 2>&1
    systemctl reload nginx
    systemctl restart xray
    systemctl is-active --quiet nginx xray
   fi
   rm -f "$t/pending"
  fi
 fi`
}

func siteXrayConfig(raw []byte, domain string) ([]byte, error) {
	doc, err := parseXray(raw)
	if err != nil {
		return nil, err
	}
	var stream, reality map[string]json.RawMessage
	if err = json.Unmarshal(doc.inbounds[doc.index]["streamSettings"], &stream); err != nil || stream == nil {
		return nil, fmt.Errorf("missing Xray stream settings")
	}
	if err = json.Unmarshal(stream["realitySettings"], &reality); err != nil || reality == nil {
		return nil, fmt.Errorf("missing REALITY settings")
	}
	reality["serverNames"], _ = json.Marshal([]string{domain})
	stream["realitySettings"], _ = json.Marshal(reality)
	doc.inbounds[doc.index]["streamSettings"], _ = json.Marshal(stream)
	doc.root["inbounds"], _ = json.Marshal(doc.inbounds)
	// Do not regenerate clients, keys, statistics policy, or unrelated inbounds.
	return json.MarshalIndent(doc.root, "", "  ")
}

func updateSiteRemote(r remoteCommand, current, desired Server, email string, commit func() error, reporter *JobReporter, beforeChange func()) error {
	template, err := validateSiteTemplate(desired.SiteTemplate)
	if err != nil {
		return err
	}
	if _, err = normalizeDuckDNS(desired.DuckDNSURL); err != nil {
		return err
	}
	if _, err = r.Run(siteRecoveryCommand(current), nil); err != nil {
		return fmt.Errorf("Восстановление предыдущего изменения сайта: %w", err)
	}
	if _, err = r.Run(recoveryCommand(), nil); err != nil {
		return fmt.Errorf("Восстановление конфигурации: %w", err)
	}
	domainChanged := current.DuckDNSURL != desired.DuckDNSURL
	if domainChanged {
		version, err := readRemote(r, runtimePath+"/installation-version")
		if err != nil || strings.TrimSpace(string(version)) != "2" {
			return fmt.Errorf("Перед сменой домена выполните синхронизацию сервера для миграции на SSH")
		}
		if email == "" {
			return fmt.Errorf("Укажите email для сертификатов в настройках")
		}
	}
	path := "deploy/index.html"
	if template != "file-hosting" {
		path = "deploy/templates/" + template + ".html"
	}
	html, err := deploymentFiles.ReadFile(path)
	if err != nil {
		return err
	}
	stageRaw, err := r.Run("umask 077; mktemp -d /usr/local/etc/xray/.orchestrator.XXXXXX", nil)
	if err != nil {
		return err
	}
	stage := strings.TrimSpace(string(stageRaw))
	if !strings.HasPrefix(stage, "/usr/local/etc/xray/.orchestrator.") || strings.ContainsAny(stage, "\n\r \t") {
		return fmt.Errorf("invalid staging directory")
	}
	defer r.Run("rm -rf -- "+shellQuote(stage), nil)
	if err = writeRemote(r, stage+"/index", html); err != nil {
		return err
	}
	if domainChanged {
		raw, err := readRemote(r, xrayConfigPath)
		if err != nil {
			return err
		}
		config, err := siteXrayConfig(raw, desired.DuckDNSURL)
		if err != nil {
			return err
		}
		nginx, err := readRemote(r, nginxSitePath)
		if err != nil {
			return err
		}
		// Preserve custom locations and TLS settings in the managed site.
		if !strings.Contains(string(nginx), current.DuckDNSURL) {
			return fmt.Errorf("В конфигурации Nginx не найден текущий домен")
		}
		nginx = []byte(strings.ReplaceAll(string(nginx), current.DuckDNSURL, desired.DuckDNSURL))
		if err = writeRemote(r, stage+"/config.json", config); err != nil {
			return err
		}
		if err = writeRemote(r, stage+"/nginx", nginx); err != nil {
			return err
		}
		if _, err = r.Run("/usr/local/bin/xray run -test -format json -config "+shellQuote(stage+"/config.json")+" >/dev/null 2>&1", nil); err != nil {
			return fmt.Errorf("Xray отклонил новый домен: %w", err)
		}
		reporter.Log("DuckDNS: привязка %s к адресу VPS…", desired.DuckDNSURL)
		command := `set -eu
 reply=$(curl -4 -fsS --connect-timeout 10 --max-time 30 --get --data-urlencode ` + shellQuote("domains="+strings.TrimSuffix(desired.DuckDNSURL, ".duckdns.org")) + ` --data-urlencode ` + shellQuote("token@"+runtimePath+"/duckdns-token") + ` https://www.duckdns.org/update)
 test "$reply" = OK`
		if _, err = r.Run(command, nil); err != nil {
			return fmt.Errorf("DuckDNS не принял новый адрес. Создайте поддомен в том же аккаунте DuckDNS и проверьте токен: %w", err)
		}
		reporter.Log("Выпуск сертификата для %s через DNS-01…", desired.DuckDNSURL)
		command = "set -eu; certbot certonly --manual --preferred-challenges dns --manual-auth-hook /opt/vps-reality/hooks/duckdns-auth.sh --manual-cleanup-hook /opt/vps-reality/hooks/duckdns-cleanup.sh --non-interactive --keep-until-expiring --agree-tos --email " + shellQuote(email) + " --cert-name " + shellQuote(desired.DuckDNSURL) + " -d " + shellQuote(desired.DuckDNSURL) + " >/dev/null 2>&1; test -s " + shellQuote("/etc/letsencrypt/live/"+desired.DuckDNSURL+"/fullchain.pem") + "; test -s " + shellQuote("/etc/letsencrypt/live/"+desired.DuckDNSURL+"/privkey.pem")
		if _, err = r.Run(command, nil); err != nil {
			return fmt.Errorf("Не удалось выпустить сертификат; прежняя конфигурация сохранена: %w", err)
		}
	}
	reporter.Log("Применение заглушки %s…", template)
	if domainChanged && beforeChange != nil {
		beforeChange()
	}
	if _, err = r.Run(siteApplyCommand(stage, desired, domainChanged), nil); err != nil {
		if _, rollbackErr := r.Run(siteRecoveryCommand(current), nil); rollbackErr != nil {
			return fmt.Errorf("Применение не завершено; повторите операцию для восстановления: %w", rollbackErr)
		}
		return fmt.Errorf("Изменение отклонено, прежняя конфигурация восстановлена: %w", err)
	}
	if err = commit(); err != nil {
		if _, rollbackErr := r.Run(siteRecoveryCommand(current), nil); rollbackErr != nil {
			return fmt.Errorf("Состояние не сохранено (%v); повторите операцию для восстановления: %w", err, rollbackErr)
		}
		return fmt.Errorf("Состояние не сохранено, изменение на VPS отменено: %w", err)
	}
	if _, err = r.Run(siteRecoveryCommand(desired), nil); err != nil {
		reporter.Warning("Настройки сохранены; маркер операции будет удалён при следующем SSH-изменении: %v", err)
	}
	return nil
}

func siteApplyCommand(stage string, desired Server, domainChanged bool) string {
	command := `set -eu
 umask 077
 t=` + shellQuote(siteTransactionPath) + `
 mkdir -p "$t"
 cp -p ` + siteIndexPath + ` "$t/index"
 rm -f "$t/domain-change"
 printf '%s\n' ` + shellQuote(desired.DuckDNSURL) + ` > "$t/target-domain"
 printf '%s\n' ` + shellQuote(defaultSiteTemplate(desired.SiteTemplate)) + ` > "$t/target-template"
 `
	if domainChanged {
		command += `cp -p ` + xrayConfigPath + ` "$t/xray"
 cp -p ` + nginxSitePath + ` "$t/nginx"
 cp -p ` + runtimePath + `/domain "$t/domain"
 touch "$t/domain-change"
 `
	}
	command += `touch "$t/pending"
 sync
 install -m 0644 ` + shellQuote(stage+"/index") + ` ` + siteIndexPath + `.next
 mv -f ` + siteIndexPath + `.next ` + siteIndexPath + `
 `
	if domainChanged {
		command += `chown --reference=` + xrayConfigPath + ` ` + shellQuote(stage+"/config.json") + `
 chmod --reference=` + xrayConfigPath + ` ` + shellQuote(stage+"/config.json") + `
 mv -f ` + shellQuote(stage+"/config.json") + ` ` + xrayConfigPath + `
 cat ` + shellQuote(stage+"/nginx") + ` > ` + nginxSitePath + `
 printf '%s\n' ` + shellQuote(desired.DuckDNSURL) + ` > ` + runtimePath + `/domain
 nginx -t >/dev/null 2>&1
 systemctl reload nginx
 systemctl restart xray
 systemctl is-active --quiet nginx xray
 `
	}
	return command
}
