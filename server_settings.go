package main

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// The state commit runs after remote validation but before removing the remote
// recovery marker. A later SSH operation resolves an interrupted commit using
// the domain and template actually saved in the master's state.
type siteManager interface {
	UpdateSite(context.Context, Server, Server, string, func() error, *JobReporter) error
}

func normalizeServerUpdate(input *UpdateServerRequest) error {
	if input.DisplayName != nil {
		value, err := normalizeServerDisplayName(*input.DisplayName)
		if err != nil {
			return err
		}
		input.DisplayName = &value
	}
	if input.DuckDNSURL != nil {
		value, err := normalizeDuckDNS(*input.DuckDNSURL)
		if err != nil {
			return err
		}
		input.DuckDNSURL = &value
	}
	if input.SiteTemplate != nil {
		value, err := validateSiteTemplate(*input.SiteTemplate)
		if err != nil {
			return err
		}
		input.SiteTemplate = &value
	}
	return nil
}

func requestedServerSettings(server Server, input UpdateServerRequest) Server {
	if input.DisplayName != nil {
		server.DisplayName = *input.DisplayName
	}
	if input.Private != nil {
		server.Private = *input.Private
	}
	if input.DuckDNSURL != nil {
		server.DuckDNSURL = *input.DuckDNSURL
	}
	if input.SiteTemplate != nil {
		server.SiteTemplate = *input.SiteTemplate
	}
	return server
}

func siteSettingsChanged(before, after Server) bool {
	return before.DuckDNSURL != after.DuckDNSURL || defaultSiteTemplate(before.SiteTemplate) != defaultSiteTemplate(after.SiteTemplate)
}

func validateServerSiteUpdate(state State, before, after Server) error {
	if !siteSettingsChanged(before, after) {
		return nil
	}
	if before.Provisioning {
		return fmt.Errorf("Сначала завершите установку сервера")
	}
	for _, other := range state.Servers {
		if other.ID != before.ID && strings.EqualFold(other.DuckDNSURL, after.DuckDNSURL) {
			return fmt.Errorf("Этот DuckDNS-адрес уже используется другим сервером")
		}
	}
	return nil
}

func updatedLinkURI(raw, domain string) (string, error) {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "vless" || parsed.User == nil || parsed.Hostname() == "" {
		return "", fmt.Errorf("Некорректная сохранённая VLESS-ссылка; сначала синхронизируйте сервер")
	}
	port := parsed.Port()
	if port == "" {
		port = "443"
	}
	parsed.Host = domain + ":" + port
	query := parsed.Query()
	query.Set("sni", domain)
	parsed.RawQuery = query.Encode()
	return parsed.String(), nil
}

func (app *App) updateServerSite(ctx context.Context, id string, input UpdateServerRequest, reporter *JobReporter) error {
	app.operationMu.Lock()
	defer app.operationMu.Unlock()
	state := app.store.Snapshot()
	current, _ := findServer(&state, id)
	if current == nil {
		return errServerNotFound
	}
	desired := requestedServerSettings(*current, input)
	if err := validateServerSiteUpdate(state, *current, desired); err != nil {
		return err
	}
	// Precompute links before touching the VPS. UUID, keys, subscription tokens,
	// assignment and link status all stay unchanged during a hostname change.
	uris := map[string]string{}
	if current.DuckDNSURL != desired.DuckDNSURL {
		for _, user := range state.Users {
			if link, ok := user.Links[id]; ok && link.URI != "" {
				uri, err := updatedLinkURI(link.URI, desired.DuckDNSURL)
				if err != nil {
					return err
				}
				uris[user.ID] = uri
			}
		}
	}
	commit := func() error {
		return app.store.Update(func(state *State) error {
			target, _ := findServer(state, id)
			if target == nil {
				return errServerNotFound
			}
			target.DisplayName = desired.DisplayName
			target.Private = desired.Private
			target.DuckDNSURL = desired.DuckDNSURL
			target.SiteTemplate = defaultSiteTemplate(desired.SiteTemplate)
			target.UpdatedAt = time.Now().UTC()
			for i := range state.Users {
				user := &state.Users[i]
				if link, ok := user.Links[id]; ok {
					if uri, changed := uris[user.ID]; changed {
						link.URI = uri
						link.UpdatedAt = target.UpdatedAt
					}
					if current.Private != desired.Private && !userAllowsServer(*user, desired) {
						link.Status = "revoking"
						link.LastError = "Ожидается отзыв доступа"
					}
					user.Links[id] = link
				}
			}
			return nil
		})
	}
	if siteSettingsChanged(*current, desired) {
		manager, ok := app.servers.(siteManager)
		if !ok {
			return fmt.Errorf("Менеджер сервера не поддерживает изменение сайта")
		}
		if err := manager.UpdateSite(ctx, *current, desired, app.settings().CertbotEmail, commit, reporter); err != nil {
			return err
		}
	} else if err := commit(); err != nil {
		return err
	}
	if current.Private != desired.Private {
		app.reconcileServer(ctx, desired, reporter)
	}
	reporter.Log("Настройки сервера сохранены.")
	if current.DuckDNSURL != desired.DuckDNSURL {
		reporter.Log("Адреса в подписках обновлены. Обновите подписку на клиентских устройствах.")
	}
	return nil
}
