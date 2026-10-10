package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Executes the real installer with external services replaced by local fixtures.
// No network calls, root privileges, or host configuration changes are made.
func TestInstallerResumePreservesKeysAndClients(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq required for installer fixture")
	}
	realInstall, err := exec.LookPath("install")
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	bin := filepath.Join(root, "bin")
	if err := os.MkdirAll(bin, 0700); err != nil {
		t.Fatal(err)
	}
	write := func(path, body string) { t.Helper(); fixtureWrite(t, root, path, body, 0700) }
	write("/etc/os-release", "ID=ubuntu\nVERSION_ID=24.04\n")
	for _, path := range []string{"/opt", "/run", "/etc/nginx/sites-available", "/etc/nginx/sites-enabled"} {
		if err := os.MkdirAll(root+path, 0755); err != nil {
			t.Fatal(err)
		}
	}
	write("/bin/apt-get", "#!/bin/sh\necho apt >>\"$FIXTURE/calls\"\n")
	write("/bin/openssl", "#!/bin/sh\necho 0123456789abcdef\n")
	write("/bin/chown", "#!/bin/sh\nexit 0\n")
	write("/bin/install", `#!/usr/bin/env bash
args=()
while (($#)); do case "$1" in -o|-g) shift 2;; *) args+=("$1");shift;; esac; done
exec "$REAL_INSTALL" "${args[@]}"
`)
	write("/bin/systemctl", "#!/bin/sh\necho systemctl \"$@\" >>\"$FIXTURE/calls\"\n")
	write("/bin/ufw", "#!/bin/sh\nexit 0\n")
	write("/bin/nginx", "#!/bin/sh\nexit 0\n")
	write("/bin/sshd", `#!/bin/sh
if [ "$1" = -t ]; then exit 0; fi
cat <<CONFIG
port 22
allowusers root
pubkeyauthentication yes
authenticationmethods publickey
authorizedkeysfile .ssh/authorized_keys
authorizedkeyscommand none
trustedusercakeys none
permitrootlogin prohibit-password
passwordauthentication no
kbdinteractiveauthentication no
permitemptypasswords no
CONFIG
`)
	write("/bin/curl", `#!/bin/sh
case "$*" in
 *duckdns.org/update*) echo dns >>"$FIXTURE/calls"; if [ -f "$FIXTURE/dns-ko" ]; then echo KO; else echo OK; fi;;
 *) while [ "$#" -gt 0 ]; do if [ "$1" = -o ]; then shift; printf '#!/bin/sh\nexit 0\n' >"$1";break;fi;shift;done;;
esac
`)
	write("/usr/local/bin/xray", `#!/bin/sh
if [ "$1" = x25519 ]; then
 echo keygen >>"$FIXTURE/calls"
 echo 'PrivateKey: preserved-private'
 echo 'Password: preserved-public'
fi
`)
	write("/bin/certbot", `#!/bin/sh
echo certbot >>"$FIXTURE/calls"
[ ! -f "$FIXTURE/fail-certbot" ] || exit 17
mkdir -p "$FIXTURE/etc/letsencrypt/live/test.duckdns.org"
printf cert >"$FIXTURE/etc/letsencrypt/live/test.duckdns.org/fullchain.pem"
printf key >"$FIXTURE/etc/letsencrypt/live/test.duckdns.org/privkey.pem"
`)
	source, err := deploymentFiles.ReadFile("deploy/setup.sh")
	if err != nil {
		t.Fatal(err)
	}
	script := string(source)
	script = strings.ReplaceAll(script, "[[ ${EUID:-$(id -u)} -eq 0 ]]", "true")
	script = strings.NewReplacer("/opt/", root+"/opt/", "/run/", root+"/run/", "/usr/local/", root+"/usr/local/", "/etc/", root+"/etc/", "/root/", root+"/root/").Replace(script)
	write("/bundle/setup.sh", script)
	for _, file := range []string{"index.html", "uninstall.sh", "hooks/duckdns-auth.sh", "hooks/duckdns-cleanup.sh"} {
		data, err := deploymentFiles.ReadFile("deploy/" + file)
		if err != nil {
			t.Fatal(err)
		}
		write("/bundle/"+file, string(data))
	}
	write("/root/.ssh/authorized_keys", "ssh-ed25519 existing-owner-key\n")
	run := func(wantSuccess bool) {
		t.Helper()
		cmd := exec.Command("bash", root+"/bundle/setup.sh", "--installation-id", "test-install", "--setup-ssh-key", "ssh-ed25519 new-key", "--duckdns-token", "secret-token", "--duckdns-url", "test.duckdns.org", "--certbot-email", "test@example.org")
		cmd.Env = append(os.Environ(), "FIXTURE="+root, "REAL_INSTALL="+realInstall, "PATH="+bin+":"+os.Getenv("PATH"))
		out, err := cmd.CombinedOutput()
		t.Logf("installer output: %s", out)
		if (err == nil) != wantSuccess {
			t.Fatalf("installer: %v\n%s", err, out)
		}
		if strings.Contains(string(out), "secret-token") {
			t.Fatal("token logged")
		}
	}
	write("/dns-ko", "")
	run(false)
	if _, err = os.Stat(root + "/opt/vps-reality/runtime/setup-stages/install_xray"); !os.IsNotExist(err) {
		t.Fatal("continued after DNS rejection")
	}
	os.Remove(root + "/dns-ko")
	write("/fail-certbot", "")
	run(false)
	configPath := root + "/usr/local/etc/xray/config.json"
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	data = []byte(strings.Replace(string(data), `"clients": []`, `"clients": [{"id":"existing-uuid"}]`, 1))
	if err = os.WriteFile(configPath, data, 0600); err != nil {
		t.Fatal(err)
	}
	os.Remove(root + "/fail-certbot")
	run(true)
	run(true)
	current, _ := os.ReadFile(configPath)
	if string(current) != string(data) {
		t.Fatal("resume replaced existing client config")
	}
	calls, _ := os.ReadFile(root + "/calls")
	if strings.Count(string(calls), "keygen") != 1 || strings.Count(string(calls), "certbot\n") != 2 {
		t.Fatalf("completed stages repeated: %s", calls)
	}
	keys, _ := os.ReadFile(root + "/root/.ssh/authorized_keys")
	if !strings.Contains(string(keys), "existing-owner-key") || strings.Count(string(keys), "new-key") != 1 {
		t.Fatal("SSH keys lost or duplicated")
	}
	write("/opt/vps-reality/runtime/setup-owner", "other-owner")
	run(false)
}
