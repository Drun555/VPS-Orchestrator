package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
)

type siteFixtureRemote struct {
	localRemote
	failDNS, failCert, failCleanup bool
	dnsCalls, certCalls            int
}

func (r *siteFixtureRemote) Run(command string, input []byte) ([]byte, error) {
	if strings.Contains(command, "reply=$(curl") {
		r.dnsCalls++
		if r.failDNS {
			return nil, errors.New("DNS rejected")
		}
		return nil, nil
	}
	if strings.Contains(command, "certbot certonly") {
		r.certCalls++
		if r.failCert {
			return nil, errors.New("certificate rejected")
		}
		return nil, nil
	}
	if r.failCleanup && strings.Contains(command, "target-domain") && !strings.Contains(command, "touch") {
		if _, err := os.Stat(r.dir + siteTransactionPath + "/pending"); err == nil {
			return nil, errors.New("connection lost")
		}
	}
	return r.localRemote.Run(command, input)
}
func newSiteFixture(t *testing.T) (*siteFixtureRemote, Server, Server) {
	t.Helper()
	remote, server, _ := newRemoteFixture(t)
	server.SiteTemplate = "file-hosting"
	fixtureWrite(t, remote.dir, runtimePath+"/installation-version", "2\n", 0600)
	fixtureWrite(t, remote.dir, runtimePath+"/domain", server.DuckDNSURL+"\n", 0600)
	fixtureWrite(t, remote.dir, siteIndexPath, "original HTML", 0644)
	fixtureWrite(t, remote.dir, nginxSitePath, "server {\n server_name one.duckdns.org;\n ssl_certificate /etc/letsencrypt/live/one.duckdns.org/fullchain.pem;\n location /custom { return 404; }\n}\n", 0644)
	raw, _ := os.ReadFile(remote.dir + xrayConfigPath)
	document, err := parseXray(raw)
	if err != nil {
		t.Fatal(err)
	}
	document.settings["clients"] = json.RawMessage(`[{"id":"keep-uuid","email":"user","flow":"xtls-rprx-vision"}]`)
	document.inbounds[document.index]["settings"], _ = json.Marshal(document.settings)
	document.root["inbounds"], _ = json.Marshal(document.inbounds)
	raw, _ = json.Marshal(document.root)
	fixtureWrite(t, remote.dir, xrayConfigPath, string(raw), 0640)
	desired := server
	desired.DuckDNSURL = "two.duckdns.org"
	desired.SiteTemplate = "pioneer-archive"
	return &siteFixtureRemote{localRemote: remote}, server, desired
}
func siteFiles(t *testing.T, r *siteFixtureRemote) map[string][]byte {
	t.Helper()
	result := map[string][]byte{}
	for _, path := range []string{xrayConfigPath, nginxSitePath, runtimePath + "/domain", siteIndexPath} {
		data, err := os.ReadFile(r.dir + path)
		if err != nil {
			t.Fatal(err)
		}
		result[path] = data
	}
	return result
}
func assertSiteFiles(t *testing.T, r *siteFixtureRemote, expected map[string][]byte) {
	t.Helper()
	for path, data := range siteFiles(t, r) {
		if !bytes.Equal(data, expected[path]) {
			t.Fatalf("%s changed unexpectedly", path)
		}
	}
}
func TestSiteDomainChangePreservesClientsAndCustomConfig(t *testing.T) {
	r, current, desired := newSiteFixture(t)
	committed, captured := false, false
	err := updateSiteRemote(r, current, desired, "admin@example.org", func() error { committed = true; return nil }, fixtureReporter(), func() { captured = true })
	if err != nil {
		t.Fatal(err)
	}
	if !committed || !captured || r.dnsCalls != 1 || r.certCalls != 1 {
		t.Fatal("missing commit, traffic capture, DNS or certificate")
	}
	files := siteFiles(t, r)
	raw := string(files[xrayConfigPath])
	for _, expected := range []string{"keep-uuid", "keep-secret", "two.duckdns.org", "127.0.0.1:10085"} {
		if !strings.Contains(raw, expected) {
			t.Fatalf("Xray lost %s", expected)
		}
	}
	if !strings.Contains(string(files[nginxSitePath]), "location /custom") || strings.Contains(string(files[nginxSitePath]), current.DuckDNSURL) {
		t.Fatal("Nginx update lost custom location or retained old domain")
	}
	if string(files[runtimePath+"/domain"]) != desired.DuckDNSURL+"\n" {
		t.Fatal("runtime domain not updated")
	}
	expected, _ := deploymentFiles.ReadFile("deploy/templates/pioneer-archive.html")
	if !bytes.Equal(files[siteIndexPath], expected) {
		t.Fatal("wrong installed HTML")
	}
	if _, err := os.Stat(r.dir + siteTransactionPath + "/pending"); !os.IsNotExist(err) {
		t.Fatal("pending commit remains")
	}
	info, _ := os.Stat(r.dir + xrayConfigPath)
	if info.Mode().Perm() != 0640 {
		t.Fatal("Xray permissions changed")
	}
}
func TestSiteOnlyChangeDoesNotRestartServicesOrIssueCertificate(t *testing.T) {
	r, current, desired := newSiteFixture(t)
	desired.DuckDNSURL = current.DuckDNSURL
	before := siteFiles(t, r)
	if err := updateSiteRemote(r, current, desired, "", func() error { return nil }, fixtureReporter(), nil); err != nil {
		t.Fatal(err)
	}
	after := siteFiles(t, r)
	for _, path := range []string{xrayConfigPath, nginxSitePath, runtimePath + "/domain"} {
		if !bytes.Equal(before[path], after[path]) {
			t.Fatalf("%s changed", path)
		}
	}
	if r.dnsCalls != 0 || r.certCalls != 0 {
		t.Fatal("site-only update contacted DNS/CA")
	}
	if log, _ := os.ReadFile(r.dir + "/service-log"); len(log) > 0 {
		t.Fatal("site-only update restarted services")
	}
}
func TestSiteChangeFailuresRestoreLiveFiles(t *testing.T) {
	for _, failure := range []string{"dns", "certificate", "xray-validation", "restart", "nginx", "state"} {
		t.Run(failure, func(t *testing.T) {
			r, current, desired := newSiteFixture(t)
			before := siteFiles(t, r)
			switch failure {
			case "dns":
				r.failDNS = true
			case "certificate":
				r.failCert = true
			case "xray-validation":
				fixtureWrite(t, r.dir, "/reject-config", "", 0600)
			case "restart":
				fixtureWrite(t, r.dir, "/fail-restart", "", 0600)
			case "nginx":
				fixtureWrite(t, r.dir, "/bin/nginx", "#!/bin/sh\nif [ -f \"$FIXTURE/fail-nginx\" ]; then rm \"$FIXTURE/fail-nginx\"; exit 1; fi\n", 0700)
				fixtureWrite(t, r.dir, "/fail-nginx", "", 0600)
			}
			committed := false
			err := updateSiteRemote(r, current, desired, "admin@example.org", func() error {
				if failure == "state" {
					return errors.New("disk full")
				}
				committed = true
				return nil
			}, fixtureReporter(), nil)
			if err == nil || committed {
				t.Fatal("failed change committed", err)
			}
			assertSiteFiles(t, r, before)
		})
	}
}
func TestSiteInterruptedCommitRecovery(t *testing.T) {
	for _, saved := range []bool{false, true} {
		name := "before-state-save"
		if saved {
			name = "after-state-save"
		}
		t.Run(name, func(t *testing.T) {
			r, current, desired := newSiteFixture(t)
			before := siteFiles(t, r)
			r.failCleanup = true
			err := updateSiteRemote(r, current, desired, "admin@example.org", func() error {
				if !saved {
					return errors.New("state write interrupted")
				}
				return nil
			}, fixtureReporter(), nil)
			if saved && err != nil {
				t.Fatal(err)
			}
			if !saved && err == nil {
				t.Fatal("missing state error")
			}
			r.failCleanup = false
			actual := current
			if saved {
				actual = desired
			}
			if _, err = r.Run(siteRecoveryCommand(actual), nil); err != nil {
				t.Fatal(err)
			}
			if saved {
				if string(siteFiles(t, r)[runtimePath+"/domain"]) != desired.DuckDNSURL+"\n" {
					t.Fatal("committed change rolled back")
				}
			} else {
				assertSiteFiles(t, r, before)
			}
			if _, err = os.Stat(r.dir + siteTransactionPath + "/pending"); !os.IsNotExist(err) {
				t.Fatal("recovery marker remains")
			}
		})
	}
}
