package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/user"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/eordano/cccp/internal/config"
)

const reviewUsage = `Usage:
  cccp review list [--all]                 Pending knowledge proposals (--all: every
                                           non-transcript version since the gate,
                                           including archived privileged writes).
  cccp review show PATH VERSION            Provenance, pre-check verdict, diff
                                           against the serving version, history.
  cccp review approve PATH VERSION [--note TEXT]
                                           Serve VERSION to every host; approving
                                           an older version is a rollback.
  cccp review reject PATH VERSION [--note TEXT]
                                           Never serve VERSION (also withdraws an
                                           earlier approval of it).
PATH is the wire path the list prints (agent/<name>/... for non-claude agents).
`

type reviewItem struct {
	Path       string `json:"path"`
	Version    int64  `json:"version"`
	Class      string `json:"class"`
	Status     string `json:"status"`
	SHA        string `json:"sha"`
	Deleted    bool   `json:"deleted"`
	HostID     string `json:"host_id"`
	Host       string `json:"host"`
	Agent      string `json:"agent"`
	Sessions   string `json:"sessions"`
	IngestedAt int64  `json:"ingested_at"`
	Superseded bool   `json:"superseded"`
	Effective  int64  `json:"effective"`
	Precheck   *struct {
		Verdict string `json:"Verdict"`
		Detail  string `json:"Detail"`
	} `json:"precheck"`
}

type reviewDecision struct {
	Version   int64  `json:"Version"`
	Decision  string `json:"Decision"`
	Reviewer  string `json:"Reviewer"`
	Note      string `json:"Note"`
	DecidedAt int64  `json:"DecidedAt"`
}

type reviewDetail struct {
	Item      reviewItem       `json:"item"`
	Content   string           `json:"content"`
	Binary    bool             `json:"binary"`
	Diff      string           `json:"diff"`
	DiffBase  int64            `json:"diff_base"`
	History   []reviewItem     `json:"history"`
	Decisions []reviewDecision `json:"decisions"`
}

func runReview(ctx context.Context, args []string) error {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, reviewUsage)
		return errors.New("review: missing subcommand")
	}
	sub, rest := args[0], args[1:]
	fs, cli := newFlagSet("review " + sub)
	all := fs.Bool("all", false, "list: include decided, legacy and archived versions")
	note := fs.String("note", "", "approve/reject: note recorded with the decision")
	var pos []string
	for len(rest) > 0 {
		if err := fs.Parse(rest); err != nil {
			return err
		}
		rest = fs.Args()
		if len(rest) > 0 {
			pos = append(pos, rest[0])
			rest = rest[1:]
		}
	}
	cfg, err := config.Resolve(fs, cli)
	if err != nil {
		return err
	}
	if cfg.URL == "" {
		return errors.New("server URL is required (--url, $CCCP_URL, or config.url)")
	}
	if cfg.Token == "" {
		if cfg.TokenFileErr != nil {
			return fmt.Errorf("read token file: %w", cfg.TokenFileErr)
		}
		return errors.New("token is required (--token, --token-file, $CCCP_TOKEN, or config token/token_file)")
	}
	rc := reviewClient{base: strings.TrimRight(cfg.URL, "/"), token: cfg.Token}

	switch sub {
	case "list":
		q := ""
		if *all {
			q = "?all=1"
		}
		var items []reviewItem
		if err := rc.get(ctx, "/review/api/list"+q, &items); err != nil {
			return err
		}
		printReviewList(items)
		return nil
	case "show":
		path, v, err := pathVersion(pos)
		if err != nil {
			return err
		}
		var d reviewDetail
		if err := rc.get(ctx, "/review/api/show?path="+url.QueryEscape(path)+"&version="+strconv.FormatInt(v, 10), &d); err != nil {
			return err
		}
		printReviewDetail(d)
		return nil
	case "approve", "reject":
		path, v, err := pathVersion(pos)
		if err != nil {
			return err
		}
		decision := map[string]string{"approve": "approved", "reject": "rejected"}[sub]
		body, _ := json.Marshal(map[string]any{
			"path": path, "version": v, "decision": decision, "note": *note, "reviewer": reviewerName(),
		})
		if err := rc.post(ctx, "/review/api/decide", body); err != nil {
			return err
		}
		fmt.Printf("%s %s v%d\n", decision, path, v)
		return nil
	}
	fmt.Fprint(os.Stderr, reviewUsage)
	return fmt.Errorf("review: unknown subcommand %q", sub)
}

func pathVersion(pos []string) (string, int64, error) {
	if len(pos) != 2 {
		return "", 0, errors.New("want PATH VERSION")
	}
	v, err := strconv.ParseInt(strings.TrimPrefix(pos[1], "v"), 10, 64)
	if err != nil {
		return "", 0, fmt.Errorf("version %q: %w", pos[1], err)
	}
	return pos[0], v, nil
}

func reviewerName() string {
	name := "unknown"
	if u, err := user.Current(); err == nil {
		name = u.Username
	}
	if h, err := os.Hostname(); err == nil {
		name += "@" + h
	}
	return "cli:" + name
}

type reviewClient struct {
	base, token string
}

func (rc reviewClient) do(ctx context.Context, method, path string, body []byte) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, rc.base+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+rc.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s %s: %d %s", method, path, resp.StatusCode, strings.TrimSpace(string(b)))
	}
	return b, nil
}

func (rc reviewClient) get(ctx context.Context, path string, out any) error {
	b, err := rc.do(ctx, "GET", path, nil)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, out)
}

func (rc reviewClient) post(ctx context.Context, path string, body []byte) error {
	_, err := rc.do(ctx, "POST", path, body)
	return err
}

func when(ns int64) string {
	if ns == 0 {
		return "-"
	}
	return time.Unix(0, ns).Local().Format("2006-01-02 15:04")
}

func hostOf(it reviewItem) string {
	if it.Host != "" {
		return it.Host
	}
	if len(it.HostID) > 8 {
		return it.HostID[:8]
	}
	return it.HostID
}

func printReviewList(items []reviewItem) {
	if len(items) == 0 {
		fmt.Println("nothing pending")
		return
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "PATH\tV\tSTATUS\tCLASS\tHOST/AGENT\tPUSHED\tPRECHECK")
	for _, it := range items {
		status := it.Status
		if it.Deleted {
			status += " (delete)"
		}
		if it.Superseded {
			status += " (superseded)"
		}
		pc := "-"
		if it.Precheck != nil {
			pc = it.Precheck.Verdict
		}
		fmt.Fprintf(tw, "%s\t%d\t%s\t%s\t%s/%s\t%s\t%s\n",
			it.Path, it.Version, status, it.Class, hostOf(it), it.Agent, when(it.IngestedAt), pc)
	}
	tw.Flush()
}

func printReviewDetail(d reviewDetail) {
	it := d.Item
	fmt.Printf("%s v%d  %s  %s\n", it.Path, it.Version, it.Status, it.Class)
	fmt.Printf("from %s (%s) agent=%s pushed %s\n", hostOf(it), it.HostID, it.Agent, when(it.IngestedAt))
	if it.Sessions != "" {
		fmt.Printf("sessions: %s\n", it.Sessions)
	}
	if it.Effective != 0 {
		fmt.Printf("serving: v%d\n", it.Effective)
	} else {
		fmt.Println("serving: nothing approved")
	}
	if it.Precheck != nil {
		fmt.Printf("pre-check: %s\n%s\n", it.Precheck.Verdict, it.Precheck.Detail)
	}
	fmt.Println()
	switch {
	case d.Binary:
		fmt.Println("(binary content)")
	case it.Deleted:
		fmt.Println("(this version deletes the file)")
	case d.Diff == "":
		fmt.Printf("(no change against v%d)\n", d.DiffBase)
	default:
		fmt.Print(d.Diff)
	}
	fmt.Println("\nhistory:")
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	for _, h := range d.History {
		mark := ""
		if h.Version == h.Effective {
			mark = " <- serving"
		}
		sha := h.SHA
		if len(sha) > 12 {
			sha = sha[:12]
		}
		fmt.Fprintf(tw, "  v%d\t%s%s\t%s\t%s\t%s\n", h.Version, h.Status, mark, hostOf(h), when(h.IngestedAt), sha)
	}
	tw.Flush()
	if len(d.Decisions) > 0 {
		fmt.Println("decisions:")
		for _, r := range d.Decisions {
			fmt.Printf("  v%d %s by %s at %s %s\n", r.Version, r.Decision, r.Reviewer, when(r.DecidedAt), r.Note)
		}
	}
}
