package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

const statsAddress = "127.0.0.1:10085"

var errStatsDisabled = errors.New("statistics not enabled")

// Only the loopback StatsService is exposed; management still uses SSH.
func enableTrafficConfig(root map[string]json.RawMessage) error {
	var api struct {
		Listen   string   `json:"listen"`
		Services []string `json:"services"`
		Tag      string   `json:"tag"`
	}
	if raw, ok := root["api"]; ok {
		if err := json.Unmarshal(raw, &api); err != nil {
			return err
		}
		if api.Listen != statsAddress || (api.Tag != "orchestrator-stats" && api.Tag != "") {
			return fmt.Errorf("cannot enable statistics: unrelated Xray API configuration")
		}
		for _, service := range api.Services {
			if service != "StatsService" {
				return fmt.Errorf("cannot replace unrelated Xray API services")
			}
		}
	}
	root["api"] = json.RawMessage(`{"tag":"orchestrator-stats","listen":"127.0.0.1:10085","services":["StatsService"]}`)
	if _, ok := root["stats"]; !ok {
		root["stats"] = json.RawMessage(`{}`)
	}
	// Merge policy objects to retain timeouts, buffers and other user levels.
	object := func(raw json.RawMessage) (map[string]json.RawMessage, error) {
		m := map[string]json.RawMessage{}
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &m); err != nil {
				return nil, err
			}
		}
		if m == nil {
			m = map[string]json.RawMessage{}
		}
		return m, nil
	}
	policy, err := object(root["policy"])
	if err != nil {
		return err
	}
	levels, err := object(policy["levels"])
	if err != nil {
		return err
	}
	level, err := object(levels["0"])
	if err != nil {
		return err
	}
	level["statsUserUplink"] = json.RawMessage(`true`)
	level["statsUserDownlink"] = json.RawMessage(`true`)
	levels["0"], _ = json.Marshal(level)
	policy["levels"], _ = json.Marshal(levels)
	root["policy"], _ = json.Marshal(policy)
	return nil
}

type TrafficBytes struct {
	Upload   int64 `json:"upload"`
	Download int64 `json:"download"`
}
type TrafficSample struct {
	Epoch    string
	Counters map[string]TrafficBytes
}
type TrafficSource interface {
	Read(context.Context, Server) (TrafficSample, error)
}
type SSHTrafficSource struct{}

func (SSHTrafficSource) Read(ctx context.Context, server Server) (TrafficSample, error) {
	if server.SSHHostFingerprint == "" {
		return TrafficSample{}, fmt.Errorf("missing pinned SSH fingerprint")
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	client, _, err := dialSSH(ctx, server.SSHHost, server.SSHPort, SSHCredential{PrivateKey: server.SSHPrivateKey, Passphrase: server.SSHPassphrase}, server.SSHHostFingerprint)
	if err != nil {
		return TrafficSample{}, err
	}
	defer client.Close()
	return readTrafficRemote(sshRemote{client})
}

func readTrafficRemote(r remoteCommand) (TrafficSample, error) {
	// InvocationID changes even for a fast restart on the same boot. Check it on
	// both sides of the query to reject snapshots crossing a process restart.
	raw, err := r.Run(`set -eu
jq -e '.api.listen == "127.0.0.1:10085" and (.api.services | index("StatsService")) != null and .policy.levels["0"].statsUserUplink == true and .policy.levels["0"].statsUserDownlink == true and .stats != null' /usr/local/etc/xray/config.json >/dev/null || { printf 'disabled\n'; exit 0; }
systemctl is-active --quiet xray
before=$(systemctl show xray --property=InvocationID --value)
test -n "$before"
printf '%s\n' "$before"
/usr/local/bin/xray api statsquery --server=127.0.0.1:10085 -timeout 5 -pattern 'user>>>'
after=$(systemctl show xray --property=InvocationID --value)
test "$before" = "$after"
`, nil)
	if err != nil {
		return TrafficSample{}, err
	}
	if string(raw) == "disabled\n" {
		return TrafficSample{}, errStatsDisabled
	}
	epoch, body, ok := bytes.Cut(raw, []byte("\n"))
	if !ok || len(epoch) == 0 {
		return TrafficSample{}, fmt.Errorf("invalid statistics response")
	}
	counters, err := parseTrafficCounters(body)
	return TrafficSample{Epoch: string(epoch), Counters: counters}, err
}

func parseTrafficCounters(raw []byte) (map[string]TrafficBytes, error) {
	var response struct {
		Stat []struct {
			Name  string      `json:"name"`
			Value json.Number `json:"value"`
		} `json:"stat"`
	}
	if err := json.Unmarshal(raw, &response); err != nil {
		return nil, fmt.Errorf("invalid Xray statistics JSON")
	}
	result := map[string]TrafficBytes{}
	for _, stat := range response.Stat {
		if !strings.HasPrefix(stat.Name, "user>>>") {
			continue
		}
		name := strings.TrimPrefix(stat.Name, "user>>>")
		direction := ""
		for _, d := range []string{"uplink", "downlink"} {
			if strings.HasSuffix(name, ">>>traffic>>>"+d) {
				name = strings.TrimSuffix(name, ">>>traffic>>>"+d)
				direction = d
				break
			}
		}
		if direction == "" {
			continue
		}
		n := int64(0)
		var err error
		if stat.Value != "" {
			n, err = strconv.ParseInt(string(stat.Value), 10, 64)
		}
		if err != nil || n < 0 {
			return nil, fmt.Errorf("invalid Xray traffic counter")
		}
		b := result[name]
		if direction == "uplink" {
			b.Upload = n
		} else {
			b.Download = n
		}
		result[name] = b
	}
	return result, nil
}
