package main

import (
	"strings"
	"time"
)

const stateVersion = 2

type State struct {
	Version  int      `json:"version"`
	Settings Settings `json:"settings,omitempty"`
	Servers  []Server `json:"servers"`
	Users    []User   `json:"users"`
}

type Server struct {
	Private            bool      `json:"private,omitempty"`
	SiteTemplate       string    `json:"site_template,omitempty"`
	Provisioning       bool      `json:"provisioning,omitempty"`
	SharedDuckDNS      bool      `json:"shared_duckdns,omitempty"`
	ID                 string    `json:"id"`
	DisplayName        string    `json:"display_name,omitempty"`
	Address            string    `json:"address"`
	SSHHost            string    `json:"ssh_host"`
	SSHPort            int       `json:"ssh_port"`
	SSHPrivateKey      string    `json:"ssh_private_key"`
	SSHPassphrase      string    `json:"ssh_passphrase,omitempty"`
	SSHHostFingerprint string    `json:"ssh_host_fingerprint"`
	PublicKey          string    `json:"public_key"`
	DuckDNSURL         string    `json:"duckdns_url"`
	DuckDNSToken       string    `json:"duckdns_token"`
	Status             string    `json:"status"`
	LastError          string    `json:"last_error,omitempty"`
	CreatedAt          time.Time `json:"created_at"`
	UpdatedAt          time.Time `json:"updated_at"`
}

type User struct {
	ServerMode        string              `json:"server_mode"`
	ServerIDs         []string            `json:"server_ids"`
	ID                string              `json:"id"`
	Email             string              `json:"email"`
	SubscriptionToken string              `json:"subscription_token"`
	Links             map[string]UserLink `json:"links"`
	CreatedAt         time.Time           `json:"created_at"`
	UpdatedAt         time.Time           `json:"updated_at"`
}

type UserLink struct {
	ServerID  string    `json:"server_id"`
	ClientID  string    `json:"client_id,omitempty"`
	URI       string    `json:"uri,omitempty"`
	Status    string    `json:"status"`
	LastError string    `json:"last_error,omitempty"`
	UpdatedAt time.Time `json:"updated_at"`
}

type ServerView struct {
	Private            bool      `json:"private"`
	SiteTemplate       string    `json:"site_template"`
	Provisioning       bool      `json:"provisioning"`
	ID                 string    `json:"id"`
	DisplayName        string    `json:"display_name"`
	Address            string    `json:"address"`
	DuckDNSURL         string    `json:"duckdns_url"`
	Status             string    `json:"status"`
	LastError          string    `json:"last_error,omitempty"`
	SSHHostFingerprint string    `json:"ssh_host_fingerprint"`
	CreatedAt          time.Time `json:"created_at"`
	UpdatedAt          time.Time `json:"updated_at"`
}

type UserLinkView struct {
	ServerID  string    `json:"server_id"`
	Status    string    `json:"status"`
	LastError string    `json:"last_error,omitempty"`
	UpdatedAt time.Time `json:"updated_at"`
}

type UserView struct {
	ServerMode      string                  `json:"server_mode"`
	ServerIDs       []string                `json:"server_ids"`
	ID              string                  `json:"id"`
	Email           string                  `json:"email"`
	SubscriptionURL string                  `json:"subscription_url"`
	Links           map[string]UserLinkView `json:"links"`
	CreatedAt       time.Time               `json:"created_at"`
	UpdatedAt       time.Time               `json:"updated_at"`
}

func serverView(server Server) ServerView {
	return ServerView{
		Private:      server.Private,
		SiteTemplate: defaultSiteTemplate(server.SiteTemplate),
		Provisioning: server.Provisioning,
		ID:           server.ID, DisplayName: server.DisplayName, Address: server.Address, DuckDNSURL: server.DuckDNSURL,
		Status: server.Status, LastError: server.LastError,
		SSHHostFingerprint: server.SSHHostFingerprint,
		CreatedAt:          server.CreatedAt, UpdatedAt: server.UpdatedAt,
	}
}

func serverDisplayName(server Server) string {
	if name := strings.TrimSpace(server.DisplayName); name != "" {
		return name
	}
	if server.DuckDNSURL != "" {
		return server.DuckDNSURL
	}
	return server.Address
}

func userView(user User, baseURL string) UserView {
	links := make(map[string]UserLinkView, len(user.Links))
	for serverID, link := range user.Links {
		links[serverID] = UserLinkView{ServerID: link.ServerID, Status: link.Status, LastError: link.LastError, UpdatedAt: link.UpdatedAt}
	}
	return UserView{
		ID: user.ID, Email: user.Email, ServerMode: user.ServerMode, ServerIDs: user.ServerIDs,
		SubscriptionURL: baseURL + "/subscribe/" + user.SubscriptionToken,
		Links:           links, CreatedAt: user.CreatedAt, UpdatedAt: user.UpdatedAt,
	}
}

func userAllowsServer(user User, server Server) bool {
	if user.ServerMode == "all" || user.ServerMode == "" {
		return !server.Private
	}
	if user.ServerMode != "selected" {
		return false
	}
	for _, id := range user.ServerIDs {
		if id == server.ID {
			return true
		}
	}
	return false
}
