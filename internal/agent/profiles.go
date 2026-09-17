package agent

import (
	"path/filepath"
	"sort"
)

const DefaultAgent = "claude"

type Profile struct {
	Name       string
	DefaultDir func(home string) string
	Allowlist  []string
}

var profiles = map[string]Profile{}

func RegisterProfile(p Profile) {
	profiles[p.Name] = p
}

func LookupProfile(name string) (Profile, bool) {
	p, ok := profiles[name]
	return p, ok
}

func ProfileNames() []string {
	out := make([]string, 0, len(profiles))
	for n := range profiles {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

func WirePrefix(agentName string) string {
	if agentName == "" || agentName == DefaultAgent {
		return ""
	}
	return "agent/" + agentName + "/"
}

func init() {
	RegisterProfile(Profile{
		Name:       DefaultAgent,
		DefaultDir: func(home string) string { return filepath.Join(home, ".claude") },
		Allowlist:  DefaultAllowlist,
	})
}
