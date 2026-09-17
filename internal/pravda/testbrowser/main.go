package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/eordano/cccp/internal/pravda"
	"github.com/eordano/cccp/internal/sae"
	"github.com/eordano/cccp/internal/store"
)

func main() {
	dir, err := os.MkdirTemp("", "pravda-sae-browser-")
	must(err)
	defer os.RemoveAll(dir)
	db, err := store.Open(filepath.Join(dir, "fixture.db"))
	must(err)
	defer db.Close()
	index, err := sae.New(db.DB())
	must(err)
	names := []string{"Implementation Planning", "Verification Planning", "Evidence Gathering", "Root Cause Analysis", "Technical Justification", "Reporting Progress", "Feature With A Deliberately Long Descriptive Name"}
	for n, enc := range []string{"browser-daily", "browser-weekly"} {
		_, err = db.DB().Exec(`INSERT INTO sae_encoders(encoder,width,top_k,layer,imported_at,current,label) VALUES(?,7,3,12,?,?,?)`, enc, time.Now().Unix(), 1-n, enc)
		must(err)
		for f, name := range names {
			_, err = db.DB().Exec(`INSERT INTO sae_features VALUES(?,?,?,?,?,?)`, enc, f, name, "Synthetic browser fixture: "+name, "neutral", 85)
			must(err)
		}
	}
	count := 36
	if value, err := strconv.Atoi(os.Getenv("PRAVDA_BROWSER_MESSAGES")); err == nil && value > 0 {
		count = value
	}
	var transcript []byte
	for i := 1; i <= count; i++ {
		kind := "assistant"
		if i%3 == 1 {
			kind = "user"
		}
		text := fmt.Sprintf("Message %d. Inspect the implementation, gather evidence, then verify the result. This synthetic conversation tests SAE navigation and feature display.", i)
		line, err := json.Marshal(map[string]any{"type": kind, "uuid": fmt.Sprintf("browser-%d", i), "timestamp": time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC).Add(time.Duration(i) * time.Minute).Format(time.RFC3339), "message": map[string]any{"role": kind, "content": []map[string]string{{"type": "text", "text": text}}}})
		must(err)
		transcript = append(transcript, append(line, '\n')...)
		for n, enc := range []string{"browser-daily", "browser-weekly"} {
			if i%9 == 0 || (n == 1 && i > 12) {
				continue
			}
			acts := []sae.Activation{{Feature: i % 7, Pct: 80}, {Feature: (i + 1) % 7, Pct: 45}, {Feature: (i + 2) % 7, Pct: 20}}
			if i%8 == 0 {
				acts = []sae.Activation{{Feature: i % 7, Pct: 20}}
			}
			if i%11 == 0 {
				acts = []sae.Activation{}
			}
			raw, _ := json.Marshal(acts)
			id := sae.RecordID(kind, text)
			_, err = db.DB().Exec(`INSERT INTO sae_records(encoder,id,kind,agent,host,session,time,path,line,snippet,features,norm) VALUES(?,?,?,?,?,?,?,?,?,?,?,0)`, enc, id, kind, "claude", "fixture", "demo", "2026-09-15T12:00:00Z", "demo.jsonl", i, text, string(raw))
			must(err)
			for _, a := range acts {
				_, err = db.DB().Exec(`INSERT INTO sae_record_features VALUES(?,?,?,?)`, enc, a.Feature, id, a.Pct)
				must(err)
			}
		}
	}
	must(os.WriteFile(filepath.Join(dir, "demo.jsonl"), transcript, 0600))
	ws := pravda.NewWatchServer(pravda.NewFSStore(dir))
	ws.Sae = index
	addr := os.Getenv("PRAVDA_BROWSER_ADDR")
	if addr == "" {
		addr = "127.0.0.1:18765"
	}
	fmt.Printf("http://%s/session/demo.jsonl\n", addr)
	handler := ws.Routes()
	if asset := os.Getenv("PRAVDA_BROWSER_HTMX"); asset != "" {
		routes := http.NewServeMux()
		routes.HandleFunc("GET /htmx.min.js", func(w http.ResponseWriter, r *http.Request) { http.ServeFile(w, r, asset) })
		routes.Handle("/", handler)
		handler = routes
	}
	log.Fatal(http.ListenAndServe(addr, handler))
}

func must(err error) {
	if err != nil {
		log.Fatal(err)
	}
}
