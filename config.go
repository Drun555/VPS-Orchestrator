package main

import (
	"bufio"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

type Config struct {
	DataDir       string
	HomeDir       string
	ListenAddress string
	Port          int
	BaseURL       string
	CertbotEmail  string
}

func loadConfig() (Config, error) {
	executable, err := os.Executable()
	if err != nil {
		return Config{}, fmt.Errorf("find executable: %w", err)
	}
	if resolved, resolveErr := filepath.EvalSymlinks(executable); resolveErr == nil {
		executable = resolved
	}
	return loadConfigFrom(filepath.Join(filepath.Dir(executable), ".env"), filepath.Dir(executable))
}

func loadConfigFrom(path, homeDir string) (Config, error) {
	values, err := readEnvFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return Config{}, err
	}
	if values == nil {
		values = map[string]string{}
	}
	for _, key := range []string{"DATA_DIR", "PORT", "BASE_URL", "CERTBOT_EMAIL", "LISTEN_ADDRESS"} {
		if value, ok := os.LookupEnv(key); ok {
			values[key] = value
		}
	}
	dataDir := strings.TrimSpace(values["DATA_DIR"])
	if dataDir == "" {
		dataDir = homeDir
	}

	port := 42345
	if raw := strings.TrimSpace(values["PORT"]); raw != "" {
		port, err = strconv.Atoi(raw)
		if err != nil || port < 1 || port > 65535 {
			return Config{}, fmt.Errorf("PORT must be an integer between 1 and 65535")
		}
	}

	baseURL, err := normalizeBaseURL(values["BASE_URL"])
	if err != nil && strings.TrimSpace(values["BASE_URL"]) != "" {
		return Config{}, err
	}
	certbotEmail := strings.TrimSpace(values["CERTBOT_EMAIL"])
	if certbotEmail != "" && !validEmail(certbotEmail) {
		return Config{}, fmt.Errorf("CERTBOT_EMAIL must contain a valid email address")
	}

	listenAddress := strings.TrimSpace(values["LISTEN_ADDRESS"])
	if listenAddress == "" {
		listenAddress = "0.0.0.0"
	}
	if net.ParseIP(listenAddress) == nil && listenAddress != "localhost" {
		return Config{}, fmt.Errorf("LISTEN_ADDRESS must be an IP address or localhost")
	}

	return Config{
		DataDir:       dataDir,
		HomeDir:       homeDir,
		ListenAddress: listenAddress,
		Port:          port,
		BaseURL:       baseURL,
		CertbotEmail:  certbotEmail,
	}, nil
}

func readEnvFile(path string) (map[string]string, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	defer file.Close()

	values := make(map[string]string)
	scanner := bufio.NewScanner(file)
	lineNumber := 0
	for scanner.Scan() {
		lineNumber++
		line := strings.TrimSpace(strings.TrimSuffix(scanner.Text(), "\r"))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, found := strings.Cut(line, "=")
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		if !found || key == "" {
			return nil, fmt.Errorf("invalid .env line %d", lineNumber)
		}
		if len(value) >= 2 && ((value[0] == '"' && value[len(value)-1] == '"') || (value[0] == '\'' && value[len(value)-1] == '\'')) {
			value = value[1 : len(value)-1]
		}
		values[key] = value
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	return values, nil
}

func normalizeBaseURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	parsed, err := url.Parse(raw)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return "", fmt.Errorf("BASE_URL must be an absolute HTTP or HTTPS URL")
	}
	if parsed.RawQuery != "" || parsed.Fragment != "" || parsed.User != nil {
		return "", fmt.Errorf("BASE_URL must not contain credentials, query, or fragment")
	}
	return strings.TrimRight(parsed.String(), "/"), nil
}
