package main

import (
	"bytes"
	"io/fs"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestTemplateValidationBeforeProvisioning(t *testing.T) {
	app := newTestApp(t, State{Version: stateVersion})
	for _, value := range []string{"../index.html", "unknown", "$(id)"} {
		request := httptest.NewRequest("POST", "/api/servers", strings.NewReader(`{"site_template":"`+value+`"}`))
		response := httptest.NewRecorder()
		app.Handler().ServeHTTP(response, request)
		if response.Code != 400 {
			t.Fatalf("%s: %d", value, response.Code)
		}
	}
	if len(app.store.Snapshot().Servers) != 0 {
		t.Fatal("invalid selection created a server")
	}
	if serverView(Server{}).SiteTemplate != "file-hosting" {
		t.Fatal("legacy default missing")
	}
}

func TestInstallerSelectsEmbeddedTemplate(t *testing.T) {
	root := t.TempDir()
	// Exercise the actual shell parser and asset installation, without package or service changes.
	err := fs.WalkDir(deploymentFiles, "deploy", func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		data, err := deploymentFiles.ReadFile(path)
		if err != nil {
			return err
		}
		target := filepath.Join(root, path)
		if err = os.MkdirAll(filepath.Dir(target), 0700); err != nil {
			return err
		}
		if path == "deploy/setup.sh" {
			data = bytes.ReplaceAll(data, []byte("/opt/vps-reality"), []byte(root+"/installed"))
			data = bytes.ReplaceAll(data, []byte(`main "$@"`), []byte("parse_arguments \"$@\"\ninstall_assets"))
		}
		return os.WriteFile(target, data, 0600)
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{"", "file-hosting", "syncthing", "backup-vault", "minecraft-worlds", "../index"} {
		args := []string{root + "/deploy/setup.sh", "--installation-id", "fixture", "--setup-ssh-key", "fixture-key", "--duckdns-token", "fixture-token", "--duckdns-url", "test.duckdns.org", "--certbot-email", "test@example.org"}
		if value != "" {
			args = append(args, "--site-template", value)
		}
		out, err := exec.Command("bash", args...).CombinedOutput()
		if value == "../index" {
			if err == nil {
				t.Fatal("shell accepted invalid selection")
			}
			continue
		}
		if err != nil {
			t.Fatalf("%s: %v %s", value, err, out)
		}
		path := "deploy/index.html"
		if value != "" && value != "file-hosting" {
			path = "deploy/templates/" + value + ".html"
		}
		expected, _ := deploymentFiles.ReadFile(path)
		actual, err := os.ReadFile(root + "/installed/index.html")
		if err != nil || !bytes.Equal(actual, expected) {
			t.Fatalf("wrong installed HTML for %s: %v", value, err)
		}
	}
}
