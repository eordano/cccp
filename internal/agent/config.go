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
	if _, ok := LookupProfile(agentName); !ok {
		return nil, fmt.Errorf("unknown agent %q (known: %s)", agentName, strings.Join(ProfileNames(), ", "))
	}
	if url == "" {
		return nil, errors.New("server URL is required (--url, $CCCP_URL, or config.url)")
	}
	if token == "" {
		return nil, errors.New("token is required (--token, $CCCP_TOKEN, or config.token)")
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

func (c *Config) DefaultAllowlist() []string {
	if p, ok := LookupProfile(c.Agent); ok && p.Allowlist != nil {
		return p.Allowlist
	}
	return DefaultAllowlist
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
