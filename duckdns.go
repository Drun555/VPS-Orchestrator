package main

import (
	"fmt"
	"strconv"
	"strings"
)

func updateDuckDNSRemote(r remoteCommand, domain string, reporter *JobReporter) error {
	script, err := deploymentFiles.ReadFile("deploy/duckdns-update.sh")
	if err != nil {
		return err
	}
	command := "bash -s -- " + shellQuote(domain) + " " + shellQuote(runtimePath+"/duckdns-token")
	for attempt := 1; attempt <= 3; attempt++ {
		output, err := r.Run(command, script)
		if err != nil {
			return fmt.Errorf("Не удалось выполнить проверку DuckDNS на VPS: %w", err)
		}
		result := strings.TrimSpace(string(output))
		if result == "OK" {
			return nil
		}
		var detail string
		retry := true
		switch {
		case result == "KO":
			detail = "DuckDNS вернул KO (HTTP 200). API отклонил сочетание поддомена и сохранённого на VPS токена; причину отказа DuckDNS не сообщает"
		case result == "TOKEN_MISSING":
			detail = "На VPS отсутствует или недоступен файл токена DuckDNS"
			retry = false
		case result == "TOKEN_EMPTY":
			detail = "На VPS сохранён пустой токен DuckDNS"
			retry = false
		case strings.HasPrefix(result, "CURL:"):
			code, parseErr := strconv.Atoi(strings.TrimPrefix(result, "CURL:"))
			if parseErr != nil {
				detail = "Не удалось разобрать результат запроса DuckDNS"
				retry = false
				break
			}
			reason := map[int]string{5: "не удалось разрешить адрес прокси", 6: "не удалось разрешить www.duckdns.org", 7: "не удалось установить соединение", 28: "истекло время ожидания", 35: "ошибка TLS", 60: "ошибка проверки TLS-сертификата"}[code]
			if reason == "" {
				reason = "сбой HTTP-запроса"
			}
			detail = fmt.Sprintf("DuckDNS недоступен с VPS: %s (curl %d)", reason, code)
		case strings.HasPrefix(result, "HTTP:"):
			code, parseErr := strconv.Atoi(strings.TrimPrefix(result, "HTTP:"))
			if parseErr != nil {
				detail = "Не удалось разобрать HTTP-статус DuckDNS"
				retry = false
				break
			}
			detail = fmt.Sprintf("DuckDNS ответил HTTP %d", code)
			retry = code == 429 || code >= 500
		default:
			detail = "DuckDNS вернул неожиданный ответ вместо OK/KO"
		}
		if !retry || attempt == 3 {
			return fmt.Errorf("%s. Конфигурация VPN и сайта не изменена", detail)
		}
		reporter.Log("%s. Повтор запроса (%d/3)…", detail, attempt+1)
		if _, err := r.Run("sleep 3", nil); err != nil {
			return fmt.Errorf("Ожидание повтора DuckDNS прервано: %w", err)
		}
	}
	return nil
}
