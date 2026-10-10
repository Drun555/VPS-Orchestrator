package main

import (
	"net/netip"
	"strings"
	"testing"
)

func TestMihomoLocalNetworkRouting(t *testing.T) {
	config := string(buildMihomoConfig("https://master.example.org", "token"))
	_, rules, ok := strings.Cut(config, "\nrules:\n")
	if !ok {
		t.Fatal("rules missing")
	}
	var networks []netip.Prefix
	for _, line := range strings.Split(rules, "\n") {
		if strings.Contains(line, "RULE-SET,") || strings.Contains(line, "MATCH,") {
			break
		}
		parts := strings.Split(strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "- ")), ",")
		if len(parts) < 3 || (parts[0] != "IP-CIDR" && parts[0] != "IP-CIDR6") {
			continue
		}
		if len(parts) != 3 || parts[2] != "DIRECT" {
			t.Fatalf("local rule must allow DNS and use DIRECT: %s", line)
		}
		prefix, err := netip.ParsePrefix(parts[1])
		if err != nil {
			t.Fatal(err)
		}
		networks = append(networks, prefix)
	}
	for _, test := range []struct {
		ip     string
		direct bool
	}{
		{"10.2.3.4", true}, {"172.16.0.1", true}, {"172.31.255.254", true}, {"192.168.5.1", true},
		{"100.64.0.1", true}, {"100.127.255.254", true}, {"127.0.0.1", true}, {"169.254.1.1", true},
		{"::1", true}, {"fd12::1", true}, {"fc00::1", true}, {"fe80::1", true},
		{"172.15.255.255", false}, {"172.32.0.1", false}, {"100.128.0.1", false}, {"192.169.0.1", false},
		{"8.8.8.8", false}, {"1.1.1.1", false}, {"2606:4700:4700::1111", false},
	} {
		matched := false
		for _, prefix := range networks {
			if prefix.Contains(netip.MustParseAddr(test.ip)) {
				matched = true
			}
		}
		if matched != test.direct {
			t.Errorf("%s: DIRECT=%v, want %v", test.ip, matched, test.direct)
		}
	}
	for _, domain := range []string{"localhost", "local", "lan", "home.arpa"} {
		rule := "DOMAIN-SUFFIX," + domain + ",DIRECT"
		if index := strings.Index(rules, rule); index < 0 || index > strings.Index(rules, "RULE-SET,") {
			t.Errorf("local domain rule missing or too late: %s", domain)
		}
	}
	if !strings.HasSuffix(strings.TrimSpace(rules), "MATCH,VPN") {
		t.Fatal("VPN fallback changed")
	}
}
