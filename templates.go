package main

import "fmt"

func defaultSiteTemplate(value string) string {
	if value == "" {
		return "file-hosting"
	}
	return value
}

func validateSiteTemplate(value string) (string, error) {
	value = defaultSiteTemplate(value)
	switch value {
	case "file-hosting", "syncthing", "backup-vault", "minecraft-worlds", "media-archive", "dataset-hub", "linux-mirror", "render-relay", "pioneer-archive", "putin-video":
		return value, nil
	default:
		return "", fmt.Errorf("unknown site_template: choose file-hosting, syncthing, backup-vault, minecraft-worlds, media-archive, dataset-hub, linux-mirror, render-relay, pioneer-archive, or putin-video")
	}
}
