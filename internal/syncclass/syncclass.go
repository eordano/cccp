package syncclass

import (
	"path"
	"strings"
)

type Class int

const (
	ClassPrivileged Class = iota
	ClassKnowledge
	ClassTranscript
)

func (c Class) String() string {
	switch c {
	case ClassKnowledge:
		return "knowledge"
	case ClassTranscript:
		return "transcript"
	default:
		return "privileged"
	}
}

const GateHeader = "X-Cccp-Gate"

const PAXDeleted = "CCCP.deleted"

func Classify(wire string, mode uint32) Class {
	name, rel, ok := splitWire(wire)
	if !ok {
		return ClassPrivileged
	}
	classify, found := byAgent[Kind(name)]
	if !found {
		return ClassPrivileged
	}
	return classify(rel, mode)
}

func Kind(name string) string {
	kind, space, named := strings.Cut(name, "-")
	if !named {
		return kind
	}
	if kind == "" || kind == "claude" || !validSpace(space) {
		return ""
	}
	return kind
}

func OwnSpace(wire string) bool {
	name, _, ok := splitWire(wire)
	return ok && strings.Contains(name, "-") && Kind(name) != ""
}

func Reviewable(wire string, mode uint32) bool {
	return Classify(wire, mode) == ClassKnowledge && !OwnSpace(wire)
}

func validSpace(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
		default:
			return false
		}
	}
	return true
}

var byAgent = map[string]func(rel string, mode uint32) Class{
	"claude":   classifyClaude,
	"codex":    classifyCodex,
	"hermes":   classifyHermes,
	"opencode": classifyOpencode,
}

func splitWire(wire string) (agentName, rel string, ok bool) {
	if rest, cut := strings.CutPrefix(wire, "agent/"); cut {
		name, r, found := strings.Cut(rest, "/")
		if !found || name == "" || r == "" {
			return "", "", false
		}
		return name, r, true
	}
	return "claude", wire, wire != ""
}

func under(rel, dir string) bool { return strings.HasPrefix(rel, dir) }

func prose(rel string, mode uint32) Class {
	if mode&0o111 != 0 {
		return ClassPrivileged
	}
	switch strings.ToLower(path.Ext(rel)) {
	case ".md", ".markdown", ".txt":
		return ClassKnowledge
	}
	return ClassPrivileged
}

func isProjectMemory(rel string) bool {
	parts := strings.SplitN(rel, "/", 4)
	return len(parts) == 4 && parts[0] == "projects" && parts[2] == "memory"
}

func classifyClaude(rel string, mode uint32) Class {
	switch {
	case rel == "CLAUDE.md":
		return prose(rel, mode)
	case under(rel, "memory/"), isProjectMemory(rel),
		under(rel, "skills/"), under(rel, "commands/"), under(rel, "agents/"):
		return prose(rel, mode)
	case rel == "history.jsonl",
		under(rel, "projects/"), under(rel, "file-history/"), under(rel, "paste-cache/"),
		under(rel, "plans/"), under(rel, "tasks/"), under(rel, "todos/"):
		return ClassTranscript
	}
	return ClassPrivileged
}

func classifyCodex(rel string, mode uint32) Class {
	switch {
	case rel == "AGENTS.md", under(rel, "skills/"), under(rel, "memories/"):
		return prose(rel, mode)
	case under(rel, "sessions/"), under(rel, "archived_sessions/"),
		rel == "history.jsonl", rel == "session_index.jsonl":
		return ClassTranscript
	}
	return ClassPrivileged
}

func classifyHermes(rel string, mode uint32) Class {
	switch {
	case rel == "SOUL.md", under(rel, "memories/"):
		return prose(rel, mode)
	case under(rel, "transcripts/"):
		return ClassTranscript
	}
	return ClassPrivileged
}

func classifyOpencode(rel string, _ uint32) Class {
	if under(rel, "transcripts/") {
		return ClassTranscript
	}
	return ClassPrivileged
}
