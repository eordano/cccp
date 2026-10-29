package agent

import (
	"os"
	"path/filepath"
)

var HermesAllowlist = []string{
	"transcripts/",
	"memories/",
	"SOUL.md",
}

var HermesExclude = []string{
	"transcripts/.backfill.json",
	"transcripts/.backfill.json.tmp",
	"transcripts/.backfill.lock",
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
		Exclude:   HermesExclude,
	})
}
