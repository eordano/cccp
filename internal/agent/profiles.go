package agent

import (
	"path/filepath"
	"sort"
	"strings"

	"github.com/eordano/cccp/internal/syncclass"
)

const DefaultAgent = "claude"

type Profile struct {
	Name       string
	DefaultDir func(home string) string
	Allowlist  []string
	Exclude    []string
}

var profiles = map[string]Profile{}

func RegisterProfile(p Profile) {
	if strings.Contains(p.Name, "-") {
		panic("cccp: profile name " + p.Name + " contains '-', which separates an agent's own space")
	}
	profiles[p.Name] = p
}

func LookupProfile(name string) (Profile, bool) {
	p, ok := profiles[name]
	return p, ok
}

func ResolveProfile(agentName string) (Profile, bool) {
	return LookupProfile(syncclass.Kind(agentName))
}

func ProfileNames() []string {
	out := make([]string, 0, len(profiles))
	for n := range profiles {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

func covers(list []string, rel string) bool {
	for _, e := range list {
		if strings.HasSuffix(e, "/") {
			if strings.HasPrefix(rel, e) {
				return true
			}
		} else if rel == e {
			return true
		}
	}
	return false
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
