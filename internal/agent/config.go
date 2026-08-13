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
	HTTP      *http.Client
}

func New(root, url, token string) (*Config, error) {
	if root == "" {
		return nil, errors.New("root directory is required")
	}
	if url == "" {
		return nil, errors.New("server URL is required (--url, $CCCP_URL, or config.url)")
	}
	if token == "" {
		return nil, errors.New("token is required (--token, $CCCP_TOKEN, or config.token)")
	}
	hostID, err := LoadOrCreateHostID(root)
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
		HTTP:      &http.Client{},
	}, nil
}

func LoadOrCreateHostID(root string) (string, error) {
	dir := filepath.Join(root, "cccp")
	path := filepath.Join(dir, "host_id")
	if b, err := os.ReadFile(path); err == nil {
		id := strings.TrimSpace(string(b))
		if id != "" {
			return id, nil
		}
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("mkdir cccp dir: %w", err)
	}
	id := uuid.NewString()
	if err := os.WriteFile(path, []byte(id+"\n"), 0o600); err != nil {
		return "", fmt.Errorf("write host_id: %w", err)
	}
	return id, nil
}

func (c *Config) AuthReq(req *http.Request) {
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("X-Host-Id", c.HostID)
	if c.HostName != "" {
		req.Header.Set("X-Host-Name", c.HostName)
	}
}
