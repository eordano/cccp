package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/eordano/cccp/internal/agent"
)

type Resolved struct {
	Agent          string
	Dir            string
	URL            string
	Token          string
	TokenFile      string
	TokenFileErr   error
	TokenFromFile  bool
	Addr           string
	DB             string
	DebounceMS     int
	ChekaAllow     int
	ChekaRedact    bool
	ChekaRedactSet bool
	PushOnly       bool
	PushToken      string
	ReadTokenFile  string
	Allowlist      []string
	Source         string
}

type CLI struct {
	agent                             string
	dir, url, token, addr, db, config string
	tokenFile                         string
	port, debounceMs, chekaAllow      int
	pushToken, readTokenFile          string
	chekaRedact, pushOnly             bool

	dirSet, urlSet, tokenSet, tokenFileSet     bool
	addrSet, portSet, dbSet, cfgSet, debSet    bool
	chekaAllowSet, chekaRedactSet, pushOnlySet bool
	pushTokenSet, agentSet, readTokenFileSet   bool
}

type fileShape struct {
	Agent         *string  `json:"agent,omitempty"`
	Dir           *string  `json:"dir,omitempty"`
	URL           *string  `json:"url,omitempty"`
	Token         *string  `json:"token,omitempty"`
	TokenFile     *string  `json:"token_file,omitempty"`
	Addr          *string  `json:"addr,omitempty"`
	Port          *int     `json:"port,omitempty"`
	DB            *string  `json:"db,omitempty"`
	DebounceMs    *int     `json:"debounce_ms,omitempty"`
	ChekaAllow    *int     `json:"cheka_allow,omitempty"`
	ChekaRedact   *bool    `json:"cheka_redact,omitempty"`
	PushOnly      *bool    `json:"push_only,omitempty"`
	PushToken     *string  `json:"push_token,omitempty"`
	ReadTokenFile *string  `json:"read_token_file,omitempty"`
	Allowlist     []string `json:"allowlist,omitempty"`
}

func RegisterFlags(fs *flag.FlagSet) *CLI {
	c := &CLI{}
	fs.StringVar(&c.agent, "agent", "", "agent profile: "+strings.Join(agent.ProfileNames(), ", ")+" (default claude)")
	fs.StringVar(&c.dir, "dir", "", "config root directory (default: the agent's home, $HOME/.claude for claude)")
	fs.StringVar(&c.url, "url", "", "server URL")
	fs.StringVar(&c.url, "server-url", "", "alias for --url")
	fs.StringVar(&c.token, "token", "", "bearer token")
	fs.StringVar(&c.tokenFile, "token-file", "", "read the bearer token from this file when no token is set")
	fs.StringVar(&c.addr, "addr", "", "listen address (default :12110)")
	fs.StringVar(&c.addr, "address", "", "alias for --addr")
	fs.IntVar(&c.port, "port", 0, "listen port; overrides the port in --addr")
	fs.StringVar(&c.db, "db", "", "sqlite database path")
	fs.StringVar(&c.db, "database", "", "alias for --db")
	fs.StringVar(&c.db, "db-path", "", "alias for --db")
	fs.IntVar(&c.debounceMs, "debounce-ms", 0, "secretary debounce window in ms (default 500)")
	fs.IntVar(&c.chekaAllow, "cheka-allow", 0, "tolerate up to N secret findings (default 0; -1 disables)")
	fs.BoolVar(&c.chekaRedact, "cheka-redact", false, "redact secrets in log files instead of blocking")
	fs.BoolVar(&c.pushOnly, "push-only", false, "secretary: push local changes, never pull from the server")
	fs.StringVar(&c.pushToken, "push-token", "", "serve: optional second token accepted only on the push endpoints")
	fs.StringVar(&c.readTokenFile, "read-token-file", "", "serve: optional file holding a third token accepted only for GET/HEAD on read routes")
	fs.StringVar(&c.config, "config", "", "path to config file")
	return c
}

func Resolve(fs *flag.FlagSet, c *CLI) (*Resolved, error) {
	seen := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { seen[f.Name] = true })
	c.dirSet = seen["dir"]
	c.urlSet = seen["url"] || seen["server-url"]
	c.tokenSet = seen["token"]
	c.tokenFileSet = seen["token-file"]
	c.addrSet = seen["addr"] || seen["address"]
	c.portSet = seen["port"]
	c.dbSet = seen["db"] || seen["database"] || seen["db-path"]
	c.debSet = seen["debounce-ms"]
	c.chekaAllowSet = seen["cheka-allow"]
	c.chekaRedactSet = seen["cheka-redact"]
	c.pushOnlySet = seen["push-only"]
	c.pushTokenSet = seen["push-token"]
	c.readTokenFileSet = seen["read-token-file"]
	c.cfgSet = seen["config"]
	c.agentSet = seen["agent"]

	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	agentName := resolveStr(agent.DefaultAgent, envPtr("CCCP_AGENT"), cliPtr(c.agent, c.agentSet))
	defaultDir, err := agentDir(home, agentName)
	if err != nil {
		return nil, err
	}

	dirForCfg := defaultDir
	if v, ok := os.LookupEnv("CCCP_DIR"); ok {
		dirForCfg = v
	}
	if c.dirSet {
		dirForCfg = c.dir
	}
	cfgPath := filepath.Join(dirForCfg, "cccp", "config.json")
	if p := userConfigPath(home, agentName); p != "" {
		cfgPath = p
	}
	envCfg, envCfgSet := os.LookupEnv("CCCP_CONFIG")
	if envCfgSet {
		cfgPath = envCfg
	}
	if c.cfgSet {
		cfgPath = c.config
	}

	file, found, err := loadFile(cfgPath, c.cfgSet || envCfgSet)
	if err != nil {
		return nil, err
	}

	envPort, err := envIntPtr("CCCP_PORT")
	if err != nil {
		return nil, err
	}
	envDeb, err := envIntPtr("CCCP_DEBOUNCE_MS")
	if err != nil {
		return nil, err
	}

	r := &Resolved{}
	if found {
		r.Source = cfgPath
	}
	r.Agent = resolveStr(agent.DefaultAgent, file.Agent, envPtr("CCCP_AGENT"), cliPtr(c.agent, c.agentSet))
	if r.Agent != agentName {
		if defaultDir, err = agentDir(home, r.Agent); err != nil {
			return nil, err
		}
	}
	r.Dir = resolveStr(defaultDir, file.Dir, envPtr("CCCP_DIR"), cliPtr(c.dir, c.dirSet))
	r.URL = resolveStr("", file.URL, envPtr("CCCP_URL"), cliPtr(c.url, c.urlSet))
	r.Token = resolveStr("", file.Token, envPtr("CCCP_TOKEN"), cliPtr(c.token, c.tokenSet))
	r.TokenFile = resolveStr("", file.TokenFile, envPtr("CCCP_TOKEN_FILE"), cliPtr(c.tokenFile, c.tokenFileSet))
	if r.Token == "" && r.TokenFile != "" {
		b, err := os.ReadFile(r.TokenFile)
		if err != nil {
			r.TokenFileErr = err
		} else {
			r.Token = strings.TrimSpace(string(b))
			r.TokenFromFile = true
		}
	}
	r.PushToken = resolveStr("", file.PushToken, envPtr("CCCP_PUSH_TOKEN"), cliPtr(c.pushToken, c.pushTokenSet))
	r.ReadTokenFile = resolveStr("", file.ReadTokenFile, envPtr("CCCP_READ_TOKEN_FILE"), cliPtr(c.readTokenFile, c.readTokenFileSet))
	r.Addr = resolveStr(":12110", file.Addr, envPtr("CCCP_ADDR"), cliPtr(c.addr, c.addrSet))

	if strings.HasPrefix(r.Addr, "http://") || strings.HasPrefix(r.Addr, "https://") {
		slog.Warn("addr looks like a URL; treating as --url", "addr", r.Addr)
		if r.URL == "" || c.addrSet {
			r.URL = r.Addr
		}
		r.Addr = ":12110"
	}

	if port := resolveInt(file.Port, envPort, cliIntPtr(c.port, c.portSet)); port != nil {
		r.Addr, err = withPort(r.Addr, *port)
		if err != nil {
			return nil, err
		}
	}

	defaultDB := filepath.Join(r.Dir, "cccp", "store.sqlite")
	r.DB = resolveStr(defaultDB, file.DB, envPtr("CCCP_DB"), cliPtr(c.db, c.dbSet))

	r.DebounceMS = 500
	if deb := resolveInt(file.DebounceMs, envDeb, cliIntPtr(c.debounceMs, c.debSet)); deb != nil {
		r.DebounceMS = *deb
	}

	envChekaAllow, err := envIntPtr("CCCP_CHEKA_ALLOW")
	if err != nil {
		return nil, err
	}
	if ca := resolveInt(file.ChekaAllow, envChekaAllow, cliIntPtr(c.chekaAllow, c.chekaAllowSet)); ca != nil {
		r.ChekaAllow = *ca
	}

	envChekaRedact, err := envBoolPtr("CCCP_CHEKA_REDACT")
	if err != nil {
		return nil, err
	}
	if cr := resolveBool(file.ChekaRedact, envChekaRedact, cliBoolPtr(c.chekaRedact, c.chekaRedactSet)); cr != nil {
		r.ChekaRedact = *cr
		r.ChekaRedactSet = true
	}

	envPushOnly, err := envBoolPtr("CCCP_PUSH_ONLY")
	if err != nil {
		return nil, err
	}
	if po := resolveBool(file.PushOnly, envPushOnly, cliBoolPtr(c.pushOnly, c.pushOnlySet)); po != nil {
		r.PushOnly = *po
	}

	r.Allowlist = file.Allowlist
	return r, nil
}

func userConfigPath(home, agentName string) string {
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" {
		base = filepath.Join(home, ".config")
	}
	p := filepath.Join(base, "cccp", agentName+".json")
	if _, err := os.Stat(p); err != nil {
		return ""
	}
	return p
}

func agentDir(home, name string) (string, error) {
	p, ok := agent.ResolveProfile(name)
	if !ok {
		return "", agent.UnknownAgentError(name)
	}
	return p.DefaultDir(home), nil
}

func loadFile(path string, explicit bool) (fileShape, bool, error) {
	var f fileShape
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) && !explicit {
			return f, false, nil
		}
		return f, false, fmt.Errorf("read config %s: %w", path, err)
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&f); err != nil {
		return f, false, fmt.Errorf("parse config %s: %w", path, err)
	}
	return f, true, nil
}

func resolveStr(def string, layers ...*string) string {
	out := def
	for _, l := range layers {
		if l != nil {
			out = *l
		}
	}
	return out
}

func resolveInt(layers ...*int) *int {
	var out *int
	for _, l := range layers {
		if l != nil {
			v := *l
			out = &v
		}
	}
	return out
}

func envPtr(key string) *string {
	if v, ok := os.LookupEnv(key); ok {
		return &v
	}
	return nil
}

func cliPtr(v string, set bool) *string {
	if !set {
		return nil
	}
	out := v
	return &out
}

func cliIntPtr(v int, set bool) *int {
	if !set {
		return nil
	}
	out := v
	return &out
}

func cliBoolPtr(v bool, set bool) *bool {
	if !set {
		return nil
	}
	out := v
	return &out
}

func envIntPtr(key string) (*int, error) {
	v, ok := os.LookupEnv(key)
	if !ok {
		return nil, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", key, err)
	}
	return &n, nil
}

func envBoolPtr(key string) (*bool, error) {
	v, ok := os.LookupEnv(key)
	if !ok {
		return nil, nil
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", key, err)
	}
	return &b, nil
}

func resolveBool(layers ...*bool) *bool {
	var out *bool
	for _, l := range layers {
		if l != nil {
			v := *l
			out = &v
		}
	}
	return out
}

func withPort(addr string, port int) (string, error) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	return net.JoinHostPort(host, strconv.Itoa(port)), nil
}
