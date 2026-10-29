package main

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/eordano/cccp/internal/sae"
	"github.com/eordano/cccp/internal/store"
)

func runSae(ctx context.Context, args []string) error {
	if len(args) > 0 && args[0] == "push" {
		return runSaePush(args[1:])
	}
	if len(args) > 0 && args[0] == "prune" {
		return runSaePrune(ctx, args[1:])
	}
	if len(args) == 0 || args[0] != "import" {
		return fmt.Errorf("usage: cccp sae import --db PATH --dir RUN_DIR [--strip PREFIX] [--model ID] [--layer N]\n       cccp sae push --url URL --token TOKEN --dir RUN_DIR [--strip PREFIX] [--model ID] [--layer N]\n       cccp sae prune --db PATH [--keep-days 30] [--sae-dir DIR] [--yes]")
	}
	fs := flag.NewFlagSet("sae import", flag.ExitOnError)
	db := fs.String("db", "", "sqlite store path")
	dir := fs.String("dir", "", "nv-sae run directory (result.json, scored.safetensors, records.jsonl, names.json)")
	strip := fs.String("strip", "", "prefix removed from record source paths to get pravda session paths")
	model := fs.String("model", "", "capture model for a legacy result.json that names none (default "+sae.LegacyModel+"); must agree with result.json otherwise")
	layer := fs.Int("layer", 0, "capture layer for a legacy result.json that names none (default 19); must agree with result.json otherwise")
	label := fs.String("label", "", "label for this encoder, e.g. daily-20260912")
	current := fs.Bool("current", false, "make this encoder the one pravda tags with (default when no label is given)")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if *db == "" || *dir == "" {
		return fmt.Errorf("--db and --dir are required")
	}
	st, err := store.Open(*db)
	if err != nil {
		return err
	}
	defer st.Close()
	n, err := sae.ImportWith(ctx, st.DB(), *dir, sae.ImportOptions{StripPrefix: *strip, Model: *model, Layer: *layer, Label: *label, Promote: *current || *label == ""})
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stdout, "imported %d records from %s\n", n, *dir)
	return nil
}

func runSaePush(args []string) error {
	fs := flag.NewFlagSet("sae push", flag.ExitOnError)
	url := fs.String("url", "", "cccp server URL")
	token := fs.String("token", "", "bearer token (push token is enough)")
	dir := fs.String("dir", "", "nv-sae run directory")
	strip := fs.String("strip", "", "prefix removed from record source paths")
	model := fs.String("model", "", "capture model for a legacy result.json (see sae import)")
	layer := fs.Int("layer", 0, "capture layer for a legacy result.json (see sae import)")
	label := fs.String("label", "", "encoder label")
	current := fs.Bool("current", false, "promote to the tagging encoder")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *url == "" || *token == "" || *dir == "" {
		return fmt.Errorf("--url, --token and --dir are required")
	}
	var body bytes.Buffer
	zw := gzip.NewWriter(&body)
	mw := multipart.NewWriter(zw)
	for _, name := range []string{"result.json", "scored.safetensors", "records.jsonl", "names.json", "sessions.html"} {
		f, err := os.Open(filepath.Join(*dir, name))
		if err != nil {
			if name == "names.json" || name == "sessions.html" {
				continue
			}
			return err
		}
		part, err := mw.CreateFormFile(name, name)
		if err != nil {
			return err
		}
		if name == "records.jsonl" {
			err = copyTrimmedRecords(part, f)
		} else {
			_, err = io.Copy(part, f)
		}
		f.Close()
		if err != nil {
			return err
		}
	}
	_ = mw.WriteField("strip", *strip)
	_ = mw.WriteField("model", *model)
	if *layer != 0 {
		_ = mw.WriteField("layer", strconv.Itoa(*layer))
	}
	_ = mw.WriteField("label", *label)
	if *current || *label == "" {
		_ = mw.WriteField("current", "1")
	}
	if err := mw.Close(); err != nil {
		return err
	}
	if err := zw.Close(); err != nil {
		return err
	}
	req, err := http.NewRequest("POST", strings.TrimRight(*url, "/")+"/sae/import", &body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Content-Encoding", "gzip")
	req.Header.Set("Authorization", "Bearer "+*token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(out)))
	}
	fmt.Print(string(out))
	return nil
}

// copyTrimmedRecords forwards records.jsonl with each text cut to the snippet
// the server keeps anyway; the ids stay as nv-sae computed them.
func copyTrimmedRecords(w io.Writer, r io.Reader) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1<<20), 64<<20)
	bw := bufio.NewWriter(w)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var m map[string]json.RawMessage
		if err := json.Unmarshal(line, &m); err != nil {
			return fmt.Errorf("records.jsonl: %w", err)
		}
		if raw, ok := m["text"]; ok {
			var text string
			if err := json.Unmarshal(raw, &text); err == nil {
				if b, err := json.Marshal(sae.ClipSnippet(text)); err == nil {
					m["text"] = b
				}
			}
		}
		out, err := json.Marshal(m)
		if err != nil {
			return err
		}
		bw.Write(out)
		bw.WriteByte('\n')
	}
	if err := sc.Err(); err != nil {
		return err
	}
	return bw.Flush()
}

func runSaePrune(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("sae prune", flag.ExitOnError)
	db := fs.String("db", "", "sqlite store path")
	keep := fs.Int("keep-days", 30, "daily dictionaries younger than this stay")
	saeDir := fs.String("sae-dir", "", "also remove <sae-dir>/<label> pages of the dropped dictionaries")
	yes := fs.Bool("yes", false, "actually delete; without it the command only lists")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *db == "" {
		return fmt.Errorf("--db is required")
	}
	st, err := store.Open(*db)
	if err != nil {
		return err
	}
	defer st.Close()
	ix, err := sae.New(st.DB())
	if err != nil {
		return err
	}
	stale, err := ix.StaleDaily(ctx, time.Duration(*keep)*24*time.Hour)
	if err != nil {
		return err
	}
	if len(stale) == 0 {
		fmt.Println("nothing older than", *keep, "days")
		return nil
	}
	for _, e := range stale {
		verb := "would drop"
		if *yes {
			verb = "dropped"
			if err := ix.DeleteEncoder(ctx, e.Encoder); err != nil {
				return err
			}
			if *saeDir != "" && saeLabelOK(e.Label) {
				os.RemoveAll(filepath.Join(*saeDir, e.Label))
			}
		}
		fmt.Printf("%s %s (%s, %d records, imported %s)\n", verb, e.Label, e.Encoder[:12], e.Records, e.ImportedAt.Format("2006-01-02"))
	}
	if !*yes {
		fmt.Println("re-run with --yes to delete")
	}
	return nil
}

func saeLabelOK(label string) bool {
	if label == "" || len(label) > 64 {
		return false
	}
	for _, c := range label {
		ok := c == '.' || c == '_' || c == '-' || (c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
		if !ok {
			return false
		}
	}
	return true
}
