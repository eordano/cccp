package agent

import (
	"net/http"
	"strings"
	"testing"
)

func TestGate_OwnSpaceKnowledgeIsArchivedNotQueued(t *testing.T) {
	discardLogs(t)
	t.Setenv("HOME", t.TempDir())
	base, st := newBackendStore(t, "tok")
	fleetRoot, buzzRoot := newClientRoot(t), newClientRoot(t)
	writeFile(t, fleetRoot, "memories/MEMORY.md", "fleet memory\n")
	writeFile(t, buzzRoot, "memories/MEMORY.md", "buzz memory\n")
	writeFile(t, buzzRoot, "memories/USER.md", "buzz user\n")
	writeFile(t, buzzRoot, "SOUL.md", "You are Fizz.\n")
	fleet := newHermes(t, fleetRoot, base)
	buzz, err := New(buzzRoot, base, "tok", "hermes-buzz-fizz")
	if err != nil {
		t.Fatal(err)
	}
	pushHermes(t, fleet)
	pushHermes(t, buzz)

	pending := map[string]bool{}
	for _, e := range reviewList(t, base, false) {
		pending[e.Path] = true
	}
	if !pending["agent/hermes/memories/MEMORY.md"] {
		t.Errorf("the fleet Hermes's memory left the queue: %v", pending)
	}
	for p := range pending {
		if strings.HasPrefix(p, "agent/hermes-buzz-fizz/") {
			t.Errorf("%s is queued for review", p)
		}
	}

	archived := map[string]reviewEntry{}
	for _, e := range reviewList(t, base, true) {
		if strings.HasPrefix(e.Path, "agent/hermes-buzz-fizz/") {
			archived[e.Path] = e
		}
	}
	for _, p := range []string{"memories/MEMORY.md", "memories/USER.md", "SOUL.md"} {
		e, ok := archived["agent/hermes-buzz-fizz/"+p]
		if !ok || e.Status != "archived" || e.Class != "knowledge" {
			t.Errorf("%s in review list --all: %+v (found %v), want archived knowledge", p, e, ok)
		}
		if code := decide(t, base, "agent/hermes-buzz-fizz/"+p, 1, "approved"); code != http.StatusForbidden {
			t.Errorf("approving %s: %d, want 403", p, code)
		}
		if fv := latest(t, st, "agent/hermes-buzz-fizz/"+p); fv == nil || fv.ContentSHA == nil {
			t.Errorf("%s is not stored: %+v", p, fv)
		}
	}
	mustDecide(t, base, "agent/hermes/memories/MEMORY.md", 1, "approved")
}
