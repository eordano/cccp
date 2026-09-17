package agent

import (
	"os"
	"path/filepath"
)

func opencodeDataDir(home string) string {
	if xdg := os.Getenv("XDG_DATA_HOME"); xdg != "" {
		return filepath.Join(xdg, "opencode")
	}
	return filepath.Join(home, ".local", "share", "opencode")
}

func init() {
	RegisterProfile(Profile{
		Name:       "opencode",
		DefaultDir: opencodeDataDir,
		Allowlist:  []string{"transcripts/"},
	})
}
