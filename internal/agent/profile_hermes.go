package agent

import (
	"os"
	"path/filepath"
)

var HermesAllowlist = []string{
	"transcripts/",
	"memories/",
	"SOUL.md",
	"config.yaml",
}

func init() {
	RegisterProfile(Profile{
		Name: "hermes",
		DefaultDir: func(home string) string {
			if v := os.Getenv("HERMES_HOME"); v != "" {
				return v
			}
			return filepath.Join(home, ".hermes")
		},
		Allowlist: HermesAllowlist,
	})
}
