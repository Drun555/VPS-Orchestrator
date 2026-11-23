package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
)

type duckDNSHTTPRemote struct {
	localRemote
	endpoint string
}

func (r duckDNSHTTPRemote) Run(command string, input []byte) ([]byte, error) {
	input = bytes.ReplaceAll(input, []byte("https://www.duckdns.org/update"), []byte(r.endpoint))
	return r.localRemote.Run(command, input)
}

// Execute the shipped shell script and real curl against a local HTTP server.
// In particular, exercise token@file URL encoding rather than mocking the request.
func TestDuckDNSRequestAndDiagnostics(t *testing.T) {
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("curl required")
	}
	for _, tc := range []struct {
		name, token, body, want string
		status                  int
		missing, transient      bool
		calls                   int
	}{
		{name: "accepted", token: "test-token", body: "OK", status: 200, calls: 1},
		{name: "legacy-token-crlf", token: " test-token\r\n", body: "OK\r\n", status: 200, calls: 1},
		{name: "ko", token: "test-token", body: "KO\n", status: 200, want: "KO (HTTP 200)", calls: 3},
		{name: "temporary-ko", token: "test-token", body: "KO", status: 200, transient: true, calls: 2},
		{name: "http-503", token: "test-token", body: "service unavailable", status: 503, want: "HTTP 503", calls: 3},
		{name: "http-403", token: "test-token", body: "forbidden", status: 403, want: "HTTP 403", calls: 1},
		{name: "unexpected", token: "test-token", body: "untrusted test-token response", status: 200, want: "неожиданный ответ", calls: 3},
		{name: "missing", missing: true, want: "отсутствует", calls: 0},
		{name: "empty", token: "\r\n ", want: "пустой токен", calls: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			fixtureWrite(t, root, "/bin/sleep", "#!/bin/sh\nexit 0\n", 0700)
			if !tc.missing {
				fixtureWrite(t, root, runtimePath+"/duckdns-token", tc.token, 0600)
			}
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				n := calls.Add(1)
				if r.Method != "GET" || r.URL.Query().Get("domains") != "russian-pioneers" || r.URL.Query().Get("token") != "test-token" || r.URL.Query().Has("ip") {
					t.Error("incorrect DuckDNS request")
				}
				if tc.transient && n > 1 {
					w.WriteHeader(200)
					w.Write([]byte("OK"))
					return
				}
				w.WriteHeader(tc.status)
				w.Write([]byte(tc.body))
			}))
			defer server.Close()
			reporter := fixtureReporter()
			err := updateDuckDNSRemote(duckDNSHTTPRemote{localRemote{root}, server.URL}, "russian-pioneers.duckdns.org", reporter)
			if tc.want == "" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want %q, got %v", tc.want, err)
			}
			if got := calls.Load(); got != int32(tc.calls) {
				t.Fatalf("requests: %d, want %d", got, tc.calls)
			}
			if err != nil && strings.Contains(err.Error(), "test-token") {
				t.Fatal("token leaked in error")
			}
			for _, event := range reporter.job.Snapshot().Events {
				if strings.Contains(event.Message, "test-token") {
					t.Fatal("token leaked in event")
				}
			}
			if err == nil {
				token, err := os.ReadFile(root + runtimePath + "/duckdns-token")
				if err != nil {
					t.Fatal(err)
				}
				if string(token) != "test-token" {
					t.Fatal("successful credential not normalized for Certbot")
				}
			}
		})
	}
}
func TestDuckDNSCurlFailureIsNotReportedAsRejectedToken(t *testing.T) {
	root := t.TempDir()
	fixtureWrite(t, root, runtimePath+"/duckdns-token", "test-token", 0600)
	fixtureWrite(t, root, "/bin/curl", "#!/bin/sh\nprintf 'sensitive diagnostic test-token' >&2\nexit 6\n", 0700)
	fixtureWrite(t, root, "/bin/sleep", "#!/bin/sh\nexit 0\n", 0700)
	err := updateDuckDNSRemote(localRemote{root}, "russian-pioneers.duckdns.org", fixtureReporter())
	if err == nil || !strings.Contains(err.Error(), "curl 6") || strings.Contains(err.Error(), "test-token") || strings.Contains(err.Error(), "вернул KO") {
		t.Fatal(err)
	}
}
