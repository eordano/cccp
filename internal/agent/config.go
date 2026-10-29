package agent

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/uuid"
)

type Config struct {
	Root      string
	ServerURL string
	Token     string
	HostID    string
	HostName  string
	Agent     string
	Prefix    string
	HTTP      *http.Client
}

func New(root, url, token, agentName string) (*Config, error) {
	if root == "" {
		return nil, errors.New("root directory is required")
	}
	if agentName == "" {
		agentName = DefaultAgent
	}
	if _, ok := ResolveProfile(agentName); !ok {
		return nil, UnknownAgentError(agentName)
	}
	if url == "" {
		return nil, errors.New("server URL is required (--url, $CCCP_URL, or config.url)")
	}
	if token == "" {
		return nil, errors.New("token is required (--token, --token-file, $CCCP_TOKEN, $CCCP_TOKEN_FILE, or config token/token_file)")
	}
	hostID, err := loadOrCreateHostIDFor(root, agentName)
	if err != nil {
		return nil, err
	}
	hostName, _ := os.Hostname()
	return &Config{
		Root:      root,
		ServerURL: strings.TrimRight(url, "/"),
		Token:     token,
		HostID:    hostID,
		HostName:  hostName,
		Agent:     agentName,
		Prefix:    WirePrefix(agentName),
		HTTP:      &http.Client{},
	}, nil
}

func UnknownAgentError(agentName string) error {
	return fmt.Errorf("unknown agent %q (known: %s; a non-claude agent may name a space of its own as <agent>-<space>, lowercase letters, digits and -_.)",
		agentName, strings.Join(ProfileNames(), ", "))
}

func (c *Config) DefaultAllowlist() []string {
	if p, ok := ResolveProfile(c.Agent); ok && p.Allowlist != nil {
		return p.Allowlist
	}
	return DefaultAllowlist
}

func (c *Config) excluded(rel string) bool {
	p, ok := ResolveProfile(c.Agent)
	return ok && covers(p.Exclude, rel)
}

func (c *Config) localRel(wire string) (string, bool) {
	if c.Prefix == "" {
		if strings.HasPrefix(wire, "agent/") {
			return "", false
		}
		return wire, true
	}
	return strings.CutPrefix(wire, c.Prefix)
}

func LoadOrCreateHostID(root string) (string, error) {
	return loadOrCreateHostIDFor(root, DefaultAgent)
}

func readHostID(root string) string {
	b, err := os.ReadFile(filepath.Join(root, "cccp", "host_id"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func loadOrCreateHostIDFor(root, agentName string) (string, error) {
	dir := filepath.Join(root, "cccp")
	path := filepath.Join(dir, "host_id")
	if id := readHostID(root); id != "" {
		return id, nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("mkdir cccp dir: %w", err)
	}
	id := ""
	if agentName != DefaultAgent {
		if home, err := os.UserHomeDir(); err == nil {
			if p, ok := LookupProfile(DefaultAgent); ok {
				id = readHostID(p.DefaultDir(home))
			}
		}
	}
	if id == "" {
		id = uuid.NewString()
	}
	if err := os.WriteFile(path, []byte(id+"\n"), 0o600); err != nil {
		return "", fmt.Errorf("write host_id: %w", err)
	}
	return id, nil
}

func (c *Config) AuthReq(req *http.Request) {
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("X-Host-Id", c.HostID)
	req.Header.Set("X-Agent", c.Agent)
	if c.HostName != "" {
		req.Header.Set("X-Host-Name", c.HostName)
	}
}
