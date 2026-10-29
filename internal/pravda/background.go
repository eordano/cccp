package pravda

import (
	"context"
	"encoding/json"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/eordano/cccp/internal/store"
	"github.com/eordano/cccp/internal/syncclass"
)

const (
	bgMemory       = "memory"
	bgInstructions = "instructions"
	bgSkill        = "skill"
	bgAgentDef     = "agent"
	bgCommand      = "command"
	bgSettings     = "settings"
	bgHook         = "hook"
)

var bgKindOrder = map[string]int{bgInstructions: 0, bgMemory: 1, bgSkill: 2, bgAgentDef: 3, bgCommand: 4, bgSettings: 5, bgHook: 6}

func classifyBackground(wire string) (kind, agent, name string) {
	agent = agentOfPath(wire)
	rel := wire
	if agent != "claude" {
		rel = strings.TrimPrefix(wire, "agent/"+agent+"/")
	}
	parts := strings.Split(rel, "/")
	base := path.Base(rel)
	md := strings.HasSuffix(base, ".md")
	switch syncclass.Kind(agent) {
	case "claude":
		switch {
		case rel == "CLAUDE.md":
			return bgInstructions, agent, "~/.claude/CLAUDE.md"
		case parts[0] == "memory" && md:
			return bgMemory, agent, strings.TrimPrefix(rel, "memory/")
		case parts[0] == "projects" && len(parts) >= 4 && parts[2] == "memory" && md:
			return bgMemory, agent, strings.Join(parts[3:], "/")
		case parts[0] == "skills" && len(parts) == 3 && base == "SKILL.md":
			return bgSkill, agent, parts[1]
		case parts[0] == "agents" && len(parts) == 2 && md:
			return bgAgentDef, agent, strings.TrimSuffix(base, ".md")
		case parts[0] == "commands" && md:
			return bgCommand, agent, "/" + strings.TrimSuffix(strings.Join(parts[1:], ":"), ".md")
		case rel == "settings.json":
			return bgSettings, agent, rel
		case parts[0] == "hooks" && len(parts) >= 2:
			return bgHook, agent, strings.Join(parts[1:], "/")
		}
	case "codex":
		switch {
		case rel == "AGENTS.md":
			return bgInstructions, agent, "~/.codex/AGENTS.md"
		case parts[0] == "skills" && base == "SKILL.md" && len(parts) >= 3:
			return bgSkill, agent, parts[len(parts)-2]
		case rel == "config.toml" || rel == "hooks.json":
			return bgSettings, agent, rel
		case parts[0] == "rules" && len(parts) >= 2:
			return bgSettings, agent, rel
		}
	case "hermes":
		switch {
		case rel == "SOUL.md":
			return bgInstructions, agent, "~/.hermes/SOUL.md"
		case parts[0] == "memories" && len(parts) >= 2:
			return bgMemory, agent, strings.Join(parts[1:], "/")
		case rel == "config.yaml":
			return bgSettings, agent, rel
		}
	}
	return "", agent, ""
}

func claudeProjectDir(sessionPath string) string {
	parts := strings.Split(sessionPath, "/")
	if len(parts) >= 3 && parts[0] == "projects" {
		return "projects/" + parts[1] + "/"
	}
	return ""
}

func backgroundPrefixes(row *store.SessionRow) []string {
	if row.Agent != "claude" {
		return agentBackgroundPrefixes(row.Agent)
	}
	out := []string{"CLAUDE.md", "memory/", "skills/", "agents/", "commands/", "settings.json", "hooks/"}
	if dir := claudeProjectDir(row.Path); dir != "" {
		out = append(out, dir+"memory/")
	}
	return out
}

func agentBackgroundPrefixes(agent string) []string {
	base := "agent/" + agent + "/"
	switch syncclass.Kind(agent) {
	case "codex":
		return []string{base + "AGENTS.md", base + "skills/", base + "config.toml", base + "hooks.json", base + "rules/"}
	case "hermes":
		return []string{base + "SOUL.md", base + "memories/", base + "config.yaml"}
	}
	return nil
}

func populationPrefix(wire string) (agent, prefix string) {
	kind, agent, _ := classifyBackground(wire)
	if kind == bgMemory && agent == "claude" && strings.HasPrefix(wire, "projects/") {
		return agent, claudeProjectDir(wire)
	}
	if agent == "claude" {
		return agent, "projects/"
	}
	return agent, "agent/" + agent + "/"
}

type bgFile struct {
	Path     string `json:"path"`
	Kind     string `json:"kind"`
	Name     string `json:"name"`
	Version  int64  `json:"version,omitempty"`
	SHA      string `json:"sha,omitempty"`
	MtimeNS  int64  `json:"mtime_ns,omitempty"`
	HostID   string `json:"written_by_host_id,omitempty"`
	Host     string `json:"written_by_host,omitempty"`
	Versions int    `json:"versions,omitempty"`
	LoadedAs string `json:"loaded_as,omitempty"`
	Synced   bool   `json:"synced"`
	Invoked  bool   `json:"invoked,omitempty"`
}

func (b bgFile) Short() string {
	if len(b.SHA) > 10 {
		return b.SHA[:10]
	}
	return b.SHA
}

func (b bgFile) When() string { return fmtNS(b.MtimeNS) }

func fmtNS(ns int64) string {
	if ns <= 0 {
		return ""
	}
	return time.Unix(0, ns).Local().Format("2006-01-02 15:04")
}

func sessionBackground(ctx context.Context, st *store.Store, row *store.SessionRow, hosts map[string]string) ([]bgFile, error) {
	var f foldState
	_ = json.Unmarshal(row.State, &f)
	at := row.StartedAt
	if at == 0 {
		at = time.Now().UnixNano()
	}
	seen := map[string]bool{}
	var out []bgFile
	invoked := map[string]bool{}
	for _, n := range f.Invoked {
		invoked[n] = true
	}
	observed := map[string]string{}
	for _, in := range f.Instructions {
		if w := inSpace(wirePathOf(in.Path, ""), row.Agent); w != "" {
			observed[w] = in.Type
		}
	}
	for _, prefix := range backgroundPrefixes(row) {
		paths, err := st.VersionedPaths(ctx, prefix)
		if err != nil {
			return nil, err
		}
		for _, p := range paths {
			if seen[p] || (!strings.HasSuffix(prefix, "/") && p != prefix) {
				continue
			}
			seen[p] = true
			kind, _, name := classifyBackground(p)
			if kind == "" {
				continue
			}
			fv, err := st.VersionAt(ctx, p, row.Host, at)
			if err != nil {
				return nil, err
			}
			if fv == nil || fv.ContentSHA == nil {
				continue
			}
			out = append(out, bgFile{
				Path: p, Kind: kind, Name: name, Version: fv.Version, SHA: *fv.ContentSHA, MtimeNS: fv.MtimeNS,
				HostID: fv.HostID, Host: hostLabel(hosts, fv.HostID), LoadedAs: observed[p], Synced: true,
				Invoked: kind == bgSkill && invoked[name],
			})
		}
	}
	for _, in := range f.Instructions {
		w := inSpace(wirePathOf(in.Path, ""), row.Agent)
		if w != "" && seen[w] {
			continue
		}
		kind := bgInstructions
		if strings.Contains(in.Path, "/memory/") || in.Type == "AutoMem" {
			kind = bgMemory
		}
		out = append(out, bgFile{Path: in.Path, Kind: kind, Name: in.Path, LoadedAs: in.Type})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if bgKindOrder[out[i].Kind] != bgKindOrder[out[j].Kind] {
			return bgKindOrder[out[i].Kind] < bgKindOrder[out[j].Kind]
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

func hostLabel(hosts map[string]string, id string) string {
	if n := hosts[id]; n != "" {
		return n
	}
	return shortID(id)
}

type sessionRef struct {
	Path    string `json:"path"`
	ID      string `json:"id"`
	Title   string `json:"title,omitempty"`
	Host    string `json:"host,omitempty"`
	Started int64  `json:"started_at,omitempty"`
	Tool    string `json:"tool,omitempty"`
	Match   string `json:"match,omitempty"`
}

func (r sessionRef) When() string { return fmtNS(r.Started) }

type fileVersionView struct {
	Version    int64        `json:"version"`
	SHA        string       `json:"sha,omitempty"`
	Deleted    bool         `json:"deleted"`
	MtimeNS    int64        `json:"mtime_ns"`
	IngestedAt int64        `json:"ingested_at"`
	HostID     string       `json:"host_id"`
	Host       string       `json:"host"`
	Review     string       `json:"review"`
	WrittenBy  []sessionRef `json:"written_by,omitempty"`
	RanWith    int          `json:"sessions_ran_with"`
	Sessions   []sessionRef `json:"sessions,omitempty"`
}

func (v fileVersionView) Short() string {
	if len(v.SHA) > 10 {
		return v.SHA[:10]
	}
	return v.SHA
}

func (v fileVersionView) When() string { return fmtNS(v.MtimeNS) }

type fileHistory struct {
	Path     string            `json:"path"`
	Kind     string            `json:"kind"`
	Agent    string            `json:"agent"`
	Name     string            `json:"name"`
	Versions []fileVersionView `json:"versions"`
	Shown    int               `json:"-"`
	Content  string            `json:"-"`
	ViewV    int64             `json:"-"`
	Before   int               `json:"sessions_before_first_version"`
}

const (
	historyVersionCap  = 200
	historySessionsCap = 20
	writeMatchBefore   = int64(15 * time.Minute)
	writeMatchAfter    = int64(2 * time.Minute)
	activeSlack        = int64(time.Minute)
)

func buildFileHistory(ctx context.Context, st *store.Store, wire string) (*fileHistory, error) {
	tl, err := st.Timeline(ctx, wire)
	if err != nil || len(tl.Versions) == 0 {
		return nil, err
	}
	versions := tl.Versions
	hosts, err := st.HostNames(ctx)
	if err != nil {
		return nil, err
	}
	kind, agent, name := classifyBackground(wire)
	if name == "" {
		name = wire
	}
	h := &fileHistory{Path: wire, Kind: kind, Agent: agent, Name: name}
	writes, err := st.WritesTo(ctx, wire)
	if err != nil {
		return nil, err
	}
	popAgent, prefix := populationPrefix(wire)
	population, err := st.SessionsUnder(ctx, popAgent, prefix)
	if err != nil {
		return nil, err
	}
	byPath := map[string]*store.SessionRow{}
	for i := range population {
		byPath[population[i].Path] = &population[i]
	}
	views := make([]fileVersionView, len(versions))
	for i, v := range versions {
		vv := fileVersionView{Version: v.Version, Deleted: v.ContentSHA == nil, MtimeNS: v.MtimeNS, IngestedAt: v.IngestedAt,
			HostID: v.HostID, Host: hostLabel(hosts, v.HostID)}
		if v.ContentSHA != nil {
			vv.SHA = *v.ContentSHA
		}
		vv.Review = reviewState(tl, v)
		var best *store.SessionWrite
		for j := range writes {
			w := &writes[j]
			if w.Host != v.HostID || w.TS < v.MtimeNS-writeMatchBefore || w.TS > v.MtimeNS+writeMatchAfter {
				continue
			}
			if best == nil || abs64(w.TS-v.MtimeNS) < abs64(best.TS-v.MtimeNS) {
				best = w
			}
		}
		if best != nil {
			vv.WrittenBy = append(vv.WrittenBy, refFor(ctx, st, byPath, best.SessionPath, hosts, best.Tool, "tool call"))
		} else if live := liveAtPush(ctx, st, v); len(live) > 0 {
			for _, row := range live {
				vv.WrittenBy = append(vv.WrittenBy, sessionRefOf(row, hosts, "", "live on host at push"))
			}
		} else {
			for i := range population {
				s := &population[i]
				if s.Host == v.HostID && s.StartedAt-activeSlack <= v.MtimeNS && v.MtimeNS <= s.EndedAt+activeSlack {
					vv.WrittenBy = append(vv.WrittenBy, sessionRefOf(s, hosts, "", "active on host"))
				}
			}
			if len(vv.WrittenBy) > 3 {
				vv.WrittenBy = vv.WrittenBy[:3]
			}
		}
		views[i] = vv
	}
	for i := len(population) - 1; i >= 0; i-- {
		s := &population[i]
		at := s.StartedAt
		if at == 0 {
			continue
		}
		idx := tl.EffectiveAt(s.Host, at)
		if idx < 0 {
			h.Before++
			continue
		}
		views[idx].RanWith++
		if len(views[idx].Sessions) < historySessionsCap {
			views[idx].Sessions = append(views[idx].Sessions, sessionRefOf(s, hosts, "", ""))
		}
	}
	for i := len(views) - 1; i >= 0; i-- {
		h.Versions = append(h.Versions, views[i])
	}
	h.Shown = len(h.Versions)
	if h.Shown > historyVersionCap {
		h.Versions = h.Versions[:historyVersionCap]
	}
	return h, nil
}

func refFor(ctx context.Context, st *store.Store, byPath map[string]*store.SessionRow, p string, hosts map[string]string, tool, match string) sessionRef {
	if s := byPath[p]; s != nil {
		return sessionRefOf(s, hosts, tool, match)
	}
	if s, err := st.SessionRowByKey(ctx, p); err == nil && s != nil {
		return sessionRefOf(s, hosts, tool, match)
	}
	return sessionRef{Path: p, ID: pathStem(p), Tool: tool, Match: match}
}

func sessionRefOf(s *store.SessionRow, hosts map[string]string, tool, match string) sessionRef {
	return sessionRef{Path: s.Path, ID: s.ID, Title: s.Title, Host: hostLabel(hosts, s.Host), Started: s.StartedAt, Tool: tool, Match: match}
}

func abs64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

func reviewState(tl *store.VersionTimeline, v store.FileVersion) string {
	if d := store.Decision(tl.Reviews, v.Version); d != "" {
		return d
	}
	if tl.Legacy(v) {
		return "legacy"
	}
	if syncclass.Classify(v.Path, v.Mode) != syncclass.ClassKnowledge {
		return "privileged"
	}
	if syncclass.OwnSpace(v.Path) {
		return "archived"
	}
	return "pending"
}

func liveAtPush(ctx context.Context, st *store.Store, v store.FileVersion) []*store.SessionRow {
	meta, err := st.VersionMeta(ctx, v.Path, v.Version)
	if err != nil || meta.Sessions == "" {
		return nil
	}
	var out []*store.SessionRow
	for _, id := range strings.Split(meta.Sessions, ",") {
		if id = strings.TrimSpace(id); id == "" || len(out) >= 3 {
			continue
		}
		if row, err := st.SessionRowByKey(ctx, id); err == nil && row != nil && row.Host == v.HostID {
			out = append(out, row)
		}
	}
	return out
}
