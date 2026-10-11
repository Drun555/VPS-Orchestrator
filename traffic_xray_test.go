package main

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// Opt-in acceptance test against a real Xray binary, without any external VPS.
func TestTrafficWithRealXray(t *testing.T) {
	binaryPath := os.Getenv("XRAY_TEST_BINARY")
	if binaryPath == "" {
		t.Skip("set XRAY_TEST_BINARY for real Xray acceptance test")
	}
	port := func() int {
		l, e := net.Listen("tcp", "127.0.0.1:0")
		if e != nil {
			t.Fatal(e)
		}
		defer l.Close()
		return l.Addr().(*net.TCPAddr).Port
	}
	apiPort, vlessPort := port(), port()
	var root map[string]json.RawMessage
	json.Unmarshal([]byte(fmt.Sprintf(`{"log":{"loglevel":"none"},"inbounds":[{"tag":"vless-reality","listen":"127.0.0.1","port":%d,"protocol":"vless","settings":{"decryption":"none","clients":[{"id":"11111111-1111-4111-8111-111111111111","email":"Иван"}]}}],"outbounds":[{"protocol":"freedom"}]}`, vlessPort)), &root)
	if err := enableTrafficConfig(root); err != nil {
		t.Fatal(err)
	}
	root["api"] = json.RawMessage(fmt.Sprintf(`{"tag":"orchestrator-stats","listen":"127.0.0.1:%d","services":["StatsService"]}`, apiPort))
	config, _ := json.Marshal(root)
	path := filepath.Join(t.TempDir(), "config.json.setup.tmp")
	os.WriteFile(path, config, 0600)
	cmd := exec.Command(binaryPath, "run", "-format", "json", "-config", path)
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { cmd.Process.Kill(); cmd.Wait() }()
	query := func() ([]byte, error) {
		return exec.Command(binaryPath, "api", "statsquery", fmt.Sprintf("--server=127.0.0.1:%d", apiPort), "-timeout", "1", "-pattern", "user>>>").Output()
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := query(); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("Xray statistics listener did not start")
		}
		time.Sleep(50 * time.Millisecond)
	}
	payload := bytes.Repeat([]byte("traffic-test"), 10000)
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(payload) }))
	defer target.Close()
	addr := target.Listener.Addr().(*net.TCPAddr)
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", vlessPort), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(5 * time.Second))
	// VLESS TCP request to the loopback HTTP fixture: version, UUID, addons,
	// command, destination port, address type and IPv4 address.
	uuid, _ := hex.DecodeString("11111111111141118111111111111111")
	header := append([]byte{0}, uuid...)
	header = append(header, 0, 1)
	header = binary.BigEndian.AppendUint16(header, uint16(addr.Port))
	header = append(header, 1, 127, 0, 0, 1)
	request := append(header, []byte("GET / HTTP/1.1\r\nHost: localhost\r\nConnection: close\r\n\r\n")...)
	if _, err = conn.Write(request); err != nil {
		t.Fatal(err)
	}
	received, err := io.ReadAll(conn)
	if err != nil || !bytes.Contains(received, payload) {
		t.Fatal("VLESS traffic did not reach fixture", err)
	}
	conn.Close()
	raw, err := query()
	if err != nil {
		t.Fatal(err)
	}
	counters, err := parseTrafficCounters(raw)
	if err != nil {
		t.Fatal(err)
	}
	if counters["Иван"].Download < int64(len(payload)) || counters["Иван"].Upload == 0 {
		t.Fatal("missing per-user counters", counters)
	}
	raw, err = query()
	if err != nil {
		t.Fatal(err)
	}
	again, _ := parseTrafficCounters(raw)
	if again["Иван"] != counters["Иван"] {
		t.Fatal("reading reset the counters")
	}
}
