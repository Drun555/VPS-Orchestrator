package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

type fakeUpgradeManager struct {
	fakeManager
	ids  []string
	fail string
}

func (m *fakeUpgradeManager) UpgradePackages(_ context.Context, s Server, r *JobReporter) error {
	m.ids = append(m.ids, s.ID)
	if s.ID == m.fail {
		return fmt.Errorf("APT failed %s", s.SSHPassphrase)
	}
	return nil
}

func TestHostUpgradeAPI(t *testing.T) {
	for _, tc := range []struct {
		path string
		code int
		ids  []string
	}{
		{"/api/servers/missing/upgrade", 404, nil},
		{"/api/servers/setup/upgrade", 409, nil},
		{"/api/servers/two/upgrade", 202, []string{"two"}},
		{"/api/hosts/upgrade", 202, []string{"one", "two"}},
	} {
		t.Run(tc.path, func(t *testing.T) {
			initial := State{Version: stateVersion, Servers: []Server{
				{ID: "one", Status: "error", LastError: "sync pending"},
				{ID: "setup", Provisioning: true},
				{ID: "two", Status: "ready", Private: true},
			}}
			app := newTestApp(t, initial)
			before := app.store.Snapshot()
			manager := &fakeUpgradeManager{}
			app.servers = manager
			response := httptest.NewRecorder()
			app.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader("{}")))
			if response.Code != tc.code {
				t.Fatal(response.Code, response.Body.String())
			}
			if tc.code == 202 {
				var result map[string]string
				if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
					t.Fatal(err)
				}
				job, ok := app.jobs.Get(result["job_id"])
				if !ok {
					t.Fatal("job missing")
				}
				waitForJob(t, job)
				want := "success"
				if len(tc.ids) > 1 {
					want = "success_with_warnings"
				}
				if job.Snapshot().Status != want {
					t.Fatal(job.Snapshot())
				}
			}
			if !reflect.DeepEqual(manager.ids, tc.ids) {
				t.Fatal(manager.ids)
			}
			if !reflect.DeepEqual(app.store.Snapshot(), before) {
				t.Fatal("package upgrade changed server or user state")
			}
		})
	}
}

func TestHostUpgradeContinuesAfterFailureAndRedactsSecrets(t *testing.T) {
	app := newTestApp(t, State{Version: stateVersion, Servers: []Server{
		{ID: "one", SSHPassphrase: "secret-passphrase"}, {ID: "two"},
	}})
	manager := &fakeUpgradeManager{fail: "one"}
	app.servers = manager
	snapshot := runTestJob(t, app, func(r *JobReporter) error { return app.upgradePackages(context.Background(), "", r) })
	if snapshot.Status != "error" || !reflect.DeepEqual(manager.ids, []string{"one", "two"}) {
		t.Fatal(snapshot, manager.ids)
	}
	data, _ := json.Marshal(snapshot)
	if strings.Contains(string(data), "secret-passphrase") || !strings.Contains(string(data), "[REDACTED]") {
		t.Fatal(string(data))
	}
}

func TestHostUpgradeRevalidatesQueuedServer(t *testing.T) {
	for _, servers := range [][]Server{nil, {{ID: "one", Provisioning: true}}} {
		app := newTestApp(t, State{Version: stateVersion, Servers: servers})
		manager := &fakeUpgradeManager{}
		app.servers = manager
		snapshot := runTestJob(t, app, func(r *JobReporter) error { return app.upgradePackages(context.Background(), "one", r) })
		if snapshot.Status != "error" || len(manager.ids) != 0 {
			t.Fatal(snapshot, manager.ids)
		}
	}
}

// Execute the real script with stub commands: no packages or services are touched.
func TestHostUpgradeScript(t *testing.T) {
	for _, failure := range []string{"", "update", "upgrade", "xray"} {
		t.Run("failure="+failure, func(t *testing.T) {
			dir := t.TempDir()
			logPath := filepath.Join(dir, "calls")
			bashPath, err := exec.LookPath("bash")
			if err != nil {
				t.Fatal(err)
			}
			stub := "#!" + bashPath + `
printf '%s %s\n' "${0##*/}" "$*" >> "$UPGRADE_TEST_LOG"
if [ "${0##*/}" = apt-get ]; then
  [ "$DEBIAN_FRONTEND" = noninteractive ] && [ "$NEEDRESTART_MODE" = a ] && [ "$UCF_FORCE_CONFFOLD" = 1 ] || exit 90
fi
if [ -n "$UPGRADE_TEST_FAILURE" ] && [ "${!#}" = "$UPGRADE_TEST_FAILURE" ]; then exit 42; fi
`
			for _, name := range []string{"apt-get", "systemctl"} {
				if err := os.WriteFile(filepath.Join(dir, name), []byte(stub), 0700); err != nil {
					t.Fatal(err)
				}
			}
			command := exec.Command("bash", "-s")
			command.Stdin = strings.NewReader(hostUpgradeScript)
			command.Env = append(os.Environ(), "PATH="+dir+":"+os.Getenv("PATH"), "UPGRADE_TEST_LOG="+logPath, "UPGRADE_TEST_FAILURE="+failure)
			output, err := command.CombinedOutput()
			if (err != nil) != (failure != "") {
				t.Fatalf("%v: %s", err, output)
			}
			data, err := os.ReadFile(logPath)
			if err != nil {
				t.Fatal(err)
			}
			calls := strings.Split(strings.TrimSpace(string(data)), "\n")
			wantCount := map[string]int{"": 4, "update": 1, "upgrade": 2, "xray": 3}[failure]
			if len(calls) != wantCount {
				t.Fatal(calls)
			}
			if !strings.HasSuffix(calls[0], " update") || !strings.Contains(calls[0], "APT::Update::Error-Mode=any") {
				t.Fatal(calls)
			}
			if len(calls) > 1 && (!strings.HasSuffix(calls[1], " -y upgrade") || !strings.Contains(calls[1], "--force-confold")) {
				t.Fatal(calls)
			}
		})
	}
}
