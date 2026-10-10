package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"reflect"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
)

const xrayConfigPath = "/usr/local/etc/xray/config.json"
const runtimePath = "/opt/vps-reality/runtime"
const nginxSitePath = "/etc/nginx/sites-available/vps-reality"
const dropinPath = "/etc/systemd/system/xray.service.d/vps-reality.conf"
const transactionPath = runtimePath + "/orchestrator-transaction"

type ServerManager interface {
	Reconcile(context.Context, Server, []User, *JobReporter) (map[string]UserLink, error)
}

type SSHServerManager struct{}
type remoteCommand interface {
	Run(string, []byte) ([]byte, error)
}
type sshRemote struct{ client *ssh.Client }

func (r sshRemote) Run(command string, input []byte) ([]byte, error) {
	session, err := r.client.NewSession()
	if err != nil {
		return nil, err
	}
	defer session.Close()
	session.Stdin = bytes.NewReader(input)
	var output limitedOutput
	session.Stdout = &output
	// Do not return remote stderr: validators can echo configuration secrets.
	session.Stderr = io.Discard
	if err := session.Run(command); err != nil {
		return nil, fmt.Errorf("remote operation failed: %w", err)
	}
	return output.Bytes(), nil
}

type limitedOutput struct{ bytes.Buffer }

func (b *limitedOutput) Write(p []byte) (int, error) {
	if b.Len()+len(p) > 8*1024*1024 {
		return 0, fmt.Errorf("remote output exceeds 8 MiB")
	}
	return b.Buffer.Write(p)
}

func (SSHServerManager) Reconcile(ctx context.Context, server Server, users []User, reporter *JobReporter) (map[string]UserLink, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	client, _, err := dialSSH(ctx, server.SSHHost, server.SSHPort, SSHCredential{PrivateKey: server.SSHPrivateKey, Passphrase: server.SSHPassphrase}, server.SSHHostFingerprint)
	if err != nil {
		return nil, err
	}
	defer client.Close()
	// The same lock as the legacy CGI prevents a concurrent legacy write during migration.
	lock, err := client.NewSession()
	if err != nil {
		return nil, err
	}
	defer lock.Close()
	input, err := lock.StdinPipe()
	if err != nil {
		return nil, err
	}
	defer input.Close()
	output, err := lock.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err = lock.Start("flock -w 30 " + runtimePath + "/clients.lock sh -c 'echo locked; cat >/dev/null'"); err != nil {
		return nil, err
	}
	line, err := bufio.NewReader(output).ReadString('\n')
	if err != nil || strings.TrimSpace(line) != "locked" {
		return nil, fmt.Errorf("cannot acquire remote configuration lock")
	}
	return reconcileRemote(sshRemote{client}, server, users, reporter)
}

type remoteClient struct {
	ID    string `json:"id"`
	Email string `json:"email"`
	Flow  string `json:"flow,omitempty"`
}

type xrayDocument struct {
	root     map[string]json.RawMessage
	inbounds []map[string]json.RawMessage
	index    int
	settings map[string]json.RawMessage
	clients  []remoteClient
}

func parseXray(data []byte) (*xrayDocument, error) {
	d := &xrayDocument{index: -1}
	if err := json.Unmarshal(data, &d.root); err != nil {
		return nil, fmt.Errorf("invalid Xray JSON: %w", err)
	}
	if err := json.Unmarshal(d.root["inbounds"], &d.inbounds); err != nil {
		return nil, err
	}
	for i, inbound := range d.inbounds {
		var tag string
		_ = json.Unmarshal(inbound["tag"], &tag)
		if tag == "vless-reality" {
			if d.index >= 0 {
				return nil, fmt.Errorf("duplicate managed inbound")
			}
			d.index = i
		}
	}
	if d.index < 0 {
		return nil, fmt.Errorf("managed Xray inbound not found")
	}
	if err := json.Unmarshal(d.inbounds[d.index]["settings"], &d.settings); err != nil || d.settings == nil {
		return nil, fmt.Errorf("invalid inbound settings")
	}
	if err := json.Unmarshal(d.settings["clients"], &d.clients); err != nil {
		return nil, err
	}
	return d, nil
}
func (d *xrayDocument) encode(clients []remoteClient, migration bool) ([]byte, error) {
	d.settings["clients"], _ = json.Marshal(clients)
	d.inbounds[d.index]["settings"], _ = json.Marshal(d.settings)
	d.root["inbounds"], _ = json.Marshal(d.inbounds)
	if migration {
		// Only remove the API installed by the old bootstrap, never unrelated APIs.
		var api struct {
			Listen string `json:"listen"`
		}
		_ = json.Unmarshal(d.root["api"], &api)
		if api.Listen == "127.0.0.1:10085" {
			delete(d.root, "api")
		}
	}
	return json.MarshalIndent(d.root, "", "  ")
}

func newClientUUID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = (b[6] & 15) | 64
	b[8] = (b[8] & 63) | 128
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[:4], b[4:6], b[6:8], b[8:10], b[10:]), nil
}
func clientURI(server Server, id, email, publicKey, shortID string) string {
	q := url.Values{"encryption": {"none"}, "flow": {"xtls-rprx-vision"}, "security": {"reality"}, "sni": {server.DuckDNSURL}, "fp": {"chrome"}, "pbk": {publicKey}, "sid": {shortID}, "type": {"tcp"}}
	return (&url.URL{Scheme: "vless", User: url.User(id), Host: server.DuckDNSURL + ":443", RawQuery: q.Encode(), Fragment: email}).String()
}

func chooseClients(server Server, users []User, existing []remoteClient, publicKey, shortID string) ([]remoteClient, map[string]UserLink, error) {
	clients := []remoteClient{}
	links := map[string]UserLink{}
	for _, user := range users {
		if !userAllowsServer(user, server.ID) {
			continue
		}
		id := ""
		for _, candidate := range existing {
			if normalizeEmail(candidate.Email) != normalizeEmail(user.Email) {
				continue
			}
			if id == "" {
				id = candidate.ID
			}
			if candidate.ID == user.Links[server.ID].ClientID {
				id = candidate.ID
				break
			}
		}
		if id == "" {
			id = user.Links[server.ID].ClientID
		}
		if id == "" {
			var err error
			id, err = newClientUUID()
			if err != nil {
				return nil, nil, err
			}
		}
		clients = append(clients, remoteClient{ID: id, Email: user.Email, Flow: "xtls-rprx-vision"})
		links[user.ID] = UserLink{ServerID: server.ID, ClientID: id, URI: clientURI(server, id, user.Email, publicKey, shortID), Status: "ready", UpdatedAt: time.Now().UTC()}
	}
	return clients, links, nil
}

func readRemote(r remoteCommand, path string) ([]byte, error) {
	return r.Run("cat -- "+shellQuote(path), nil)
}
func writeRemote(r remoteCommand, path string, data []byte) error {
	_, err := r.Run("umask 077; cat > "+shellQuote(path), data)
	return err
}

// Durable backups allow the next operation to recover even after the SSH connection dies.
func recoveryCommand() string {
	return `set -eu
 t=` + shellQuote(transactionPath) + `
 if [ -f "$t/pending" ]; then
  cp -p "$t/xray" ` + xrayConfigPath + `
  if [ -f "$t/migration" ]; then
   cp -p "$t/nginx" ` + nginxSitePath + `
   cp -p "$t/dropin" ` + dropinPath + `
  fi
  systemctl daemon-reload
  nginx -t >/dev/null 2>&1
  systemctl reload nginx
  systemctl restart xray
  systemctl is-active --quiet xray
  rm -f "$t/pending"
 fi`
}

func reconcileRemote(r remoteCommand, server Server, users []User, reporter *JobReporter) (map[string]UserLink, error) {
	if _, err := r.Run(recoveryCommand(), nil); err != nil {
		return nil, fmt.Errorf("recover previous transaction: %w", err)
	}
	version, err := r.Run("if [ -f "+runtimePath+"/installation-version ]; then cat "+runtimePath+"/installation-version; else printf 1; fi", nil)
	if err != nil {
		return nil, err
	}
	v := strings.TrimSpace(string(version))
	if v != "1" && v != "2" {
		return nil, fmt.Errorf("unsupported remote installation version %q", v)
	}
	migration := v == "1"
	raw, err := readRemote(r, xrayConfigPath)
	if err != nil {
		return nil, err
	}
	document, err := parseXray(raw)
	if err != nil {
		return nil, err
	}
	existing := document.clients
	var nginx, dropin []byte
	if migration {
		reporter.Log("%s: миграция на SSH с сохранением UUID…", server.DuckDNSURL)
		legacy, err := readRemote(r, runtimePath+"/clients.json")
		if err != nil {
			return nil, err
		}
		var records struct {
			Clients []remoteClient `json:"clients"`
		}
		if err = json.Unmarshal(legacy, &records); err != nil || records.Clients == nil {
			return nil, fmt.Errorf("invalid legacy client database")
		}
		existing = append(existing, records.Clients...)
		nginx, err = readRemote(r, nginxSitePath)
		if err != nil {
			return nil, err
		}
		dropin, err = readRemote(r, dropinPath)
		if err != nil {
			return nil, err
		}
	}
	publicKey, err := readRemote(r, runtimePath+"/reality-public-key")
	if err != nil {
		return nil, err
	}
	shortID, err := readRemote(r, runtimePath+"/reality-short-id")
	if err != nil {
		return nil, err
	}
	if len(bytes.TrimSpace(publicKey)) == 0 || len(bytes.TrimSpace(shortID)) == 0 {
		return nil, fmt.Errorf("missing REALITY metadata")
	}
	clients, links, err := chooseClients(server, users, existing, strings.TrimSpace(string(publicKey)), strings.TrimSpace(string(shortID)))
	if err != nil {
		return nil, err
	}
	if !migration && reflect.DeepEqual(document.clients, clients) {
		if _, err := r.Run("set -eu; systemctl is-active --quiet xray; systemctl is-active --quiet nginx", nil); err != nil {
			return nil, fmt.Errorf("service health check: %w", err)
		}
		return links, nil
	}
	config, err := document.encode(clients, migration)
	if err != nil {
		return nil, err
	}
	stageRaw, err := r.Run("umask 077; mktemp -d /usr/local/etc/xray/.orchestrator.XXXXXX", nil)
	if err != nil {
		return nil, err
	}
	stage := strings.TrimSpace(string(stageRaw))
	if !strings.HasPrefix(stage, "/usr/local/etc/xray/.orchestrator.") || strings.ContainsAny(stage, "\n\r ") {
		return nil, fmt.Errorf("invalid staging directory")
	}
	defer r.Run("rm -rf -- "+shellQuote(stage), nil)
	if err = writeRemote(r, stage+"/config.json", config); err != nil {
		return nil, err
	}
	if _, err = r.Run("/usr/local/bin/xray run -test -config "+shellQuote(stage+"/config.json")+" >/dev/null 2>&1", nil); err != nil {
		return nil, fmt.Errorf("Xray rejected configuration: %w", err)
	}
	if migration {
		nginx, err = removeLegacyLocations(nginx)
		if err != nil {
			return nil, err
		}
		dropin = bytes.ReplaceAll(dropin, []byte("ExecStartPost=/opt/vps-reality/scripts/sync-users.sh"), nil)
		if err = writeRemote(r, stage+"/nginx", nginx); err != nil {
			return nil, err
		}
		if err = writeRemote(r, stage+"/dropin", dropin); err != nil {
			return nil, err
		}
	}
	if _, err = r.Run(transactionCommand(stage, migration), nil); err != nil {
		_, rollbackErr := r.Run(recoveryCommand(), nil)
		if rollbackErr != nil {
			return nil, fmt.Errorf("apply failed; rollback requires retry: %w", rollbackErr)
		}
		return nil, fmt.Errorf("configuration rejected; previous configuration restored: %w", err)
	}
	if migration {
		reporter.Log("%s: миграция завершена.", server.DuckDNSURL)
	}
	return links, nil
}

func transactionCommand(stage string, migration bool) string {
	command := `set -eu
 umask 077
 t=` + shellQuote(transactionPath) + `
 mkdir -p "$t"
 cp -p ` + xrayConfigPath + ` "$t/xray"
 rm -f "$t/migration"
 `
	if migration {
		command += `cp -p ` + nginxSitePath + ` "$t/nginx"
 cp -p ` + dropinPath + ` "$t/dropin"
 touch "$t/migration"
 # Keep the first pre-migration backup for manual rollback as well.
 if [ ! -d ` + runtimePath + `/pre-ssh-migration ]; then
  mkdir -m 700 ` + runtimePath + `/pre-ssh-migration
  cp -p "$t/xray" "$t/nginx" "$t/dropin" ` + runtimePath + `/clients.json ` + runtimePath + `/pre-ssh-migration/
 fi
 `
	}
	command += `touch "$t/pending"
 sync
 chown --reference=` + xrayConfigPath + ` ` + shellQuote(stage+"/config.json") + `
 chmod --reference=` + xrayConfigPath + ` ` + shellQuote(stage+"/config.json") + `
 mv -f ` + shellQuote(stage+"/config.json") + ` ` + xrayConfigPath + `
 `
	if migration {
		command += `cat ` + shellQuote(stage+"/nginx") + ` > ` + nginxSitePath + `
 cat ` + shellQuote(stage+"/dropin") + ` > ` + dropinPath + `
 nginx -t >/dev/null 2>&1
 systemctl daemon-reload
 systemctl reload nginx
 `
	}
	command += `systemctl restart xray
 systemctl is-active --quiet xray; systemctl is-active --quiet nginx
 `
	// Once the commit marker is removed, recovery must not restore the old configuration.
	command += `rm -f "$t/pending"
 `
	if migration {
		command += `rm -f /opt/vps-reality/cgi/register.sh /opt/vps-reality/cgi/list.sh /opt/vps-reality/cgi/remove.sh /opt/vps-reality/scripts/sync-users.sh ` + runtimePath + `/vps-reality-credentials ` + runtimePath + `/nginx.htpasswd
 chmod 0700 ` + runtimePath + `
 chmod 0600 ` + runtimePath + `/clients.json ` + runtimePath + `/clients.lock
 printf '2\n' > ` + runtimePath + `/installation-version
 `
	}
	return command
}

// Remove only the three known management locations; retain the camouflage site and custom configuration.
func removeLegacyLocations(data []byte) ([]byte, error) {
	lines := strings.Split(string(data), "\n")
	result := []string{}
	for i := 0; i < len(lines); i++ {
		fields := strings.Fields(lines[i])
		managed := len(fields) >= 4 && fields[0] == "location" && fields[1] == "=" && (fields[2] == "/list" || fields[2] == "/register" || fields[2] == "/remove")
		if !managed {
			result = append(result, lines[i])
			continue
		}
		depth := 0
		for {
			depth += strings.Count(lines[i], "{") - strings.Count(lines[i], "}")
			if depth == 0 {
				break
			}
			i++
			if i >= len(lines) {
				return nil, fmt.Errorf("unbalanced legacy Nginx location")
			}
		}
	}
	return []byte(strings.Join(result, "\n")), nil
}
