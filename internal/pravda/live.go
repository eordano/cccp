package pravda

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"log"
	"net/http"
	"sync"

	"github.com/gorilla/websocket"
)

type liveHub struct {
	mu   sync.Mutex
	subs map[string]map[*websocket.Conn]struct{}
}

func newLiveHub() *liveHub {
	return &liveHub{subs: map[string]map[*websocket.Conn]struct{}{}}
}

func (h *liveHub) add(path string, c *websocket.Conn) {
	h.mu.Lock()
	defer h.mu.Unlock()
	m, ok := h.subs[path]
	if !ok {
		m = map[*websocket.Conn]struct{}{}
		h.subs[path] = m
	}
	m[c] = struct{}{}
}

func (h *liveHub) remove(path string, c *websocket.Conn) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if m, ok := h.subs[path]; ok {
		delete(m, c)
		if len(m) == 0 {
			delete(h.subs, path)
		}
	}
}

func (h *liveHub) snapshot(path string) []*websocket.Conn {
	h.mu.Lock()
	defer h.mu.Unlock()
	m := h.subs[path]
	if len(m) == 0 {
		return nil
	}
	out := make([]*websocket.Conn, 0, len(m))
	for c := range m {
		out = append(out, c)
	}
	return out
}

type entryPrint struct {
	HasResult   bool
	SubAgentLen int
}

func fingerprintAll(entries []Entry) []entryPrint {
	out := make([]entryPrint, len(entries))
	for i := range entries {
		out[i] = entryPrint{
			HasResult:   entries[i].HasResult,
			SubAgentLen: len(entries[i].SubAgent),
		}
	}
	return out
}

type liveCache struct {
	mu    sync.Mutex
	paths map[string][]entryPrint
}

func newLiveCache() *liveCache {
	return &liveCache{paths: make(map[string][]entryPrint)}
}

func (c *liveCache) get(path string) []entryPrint {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.paths[path]
}

func (c *liveCache) set(path string, prints []entryPrint) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.paths[path] = prints
}

type parsedCache struct {
	mu    sync.Mutex
	items map[string]*parsedItem
}

type parsedItem struct {
	lineCount int
	cursor    int64
	version   int64
	events    []*RawEvent
	session   *Session
}

func newParsedCache() *parsedCache {
	return &parsedCache{items: make(map[string]*parsedItem)}
}

func (c *parsedCache) getCached(path string, currentVersion int64) ([]*RawEvent, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	item := c.items[path]
	if item == nil || item.version != currentVersion {
		return nil, false
	}
	return item.events, true
}

func (c *parsedCache) getCachedSession(path string, currentVersion int64) (*Session, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	item := c.items[path]
	if item == nil || item.version != currentVersion || item.session == nil {
		return nil, false
	}
	return item.session, true
}

func (c *parsedCache) setSession(path string, sess *Session) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if item := c.items[path]; item != nil {
		item.session = sess
	}
}

func (c *parsedCache) cursor(path string) int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	if item := c.items[path]; item != nil {
		return item.cursor
	}
	return 0
}

func (c *parsedCache) appendLines(path string, version, after, cursor int64, lines [][]byte) []*RawEvent {
	c.mu.Lock()
	defer c.mu.Unlock()
	prev := c.items[path]
	var events []*RawEvent
	if after > 0 && prev != nil {
		if prev.cursor != after {
			return prev.events
		}
		events = make([]*RawEvent, len(prev.events), len(prev.events)+len(lines))
		copy(events, prev.events)
	}
	for _, line := range lines {
		if len(line) == 0 {
			continue
		}
		var ev RawEvent
		if err := json.Unmarshal(line, &ev); err != nil {
			continue
		}
		ev.Raw = line
		events = append(events, &ev)
	}
	if cursor < after {
		cursor = after
	}
	c.items[path] = &parsedItem{cursor: cursor, version: version, events: events}
	return events
}

func (c *parsedCache) parse(r io.Reader, path string, version int64) ([]*RawEvent, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	prev := c.items[path]

	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 32*1024*1024)

	var events []*RawEvent
	skip := 0
	if prev != nil && prev.cursor == 0 {

		events = make([]*RawEvent, len(prev.events), len(prev.events)+16)
		copy(events, prev.events)
		skip = prev.lineCount
	}

	lineNum := 0
	for sc.Scan() {
		lineNum++
		if lineNum <= skip {
			continue
		}
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var ev RawEvent
		if err := json.Unmarshal(line, &ev); err != nil {
			continue
		}
		raw := make([]byte, len(line))
		copy(raw, line)
		ev.Raw = raw
		events = append(events, &ev)
	}

	if lineNum < skip {

		c.items[path] = nil
		return events, sc.Err()
	}

	c.items[path] = &parsedItem{lineCount: lineNum, version: version, events: events}
	return events, sc.Err()
}

func (s *WatchServer) handleLiveWS(w http.ResponseWriter, r *http.Request) {
	rel := r.PathValue("path")
	if rel == "" {
		http.Error(w, "missing path", http.StatusBadRequest)
		return
	}
	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Println("live upgrade:", err)
		return
	}
	s.live.add(rel, conn)
	defer func() {
		s.live.remove(rel, conn)
		conn.Close()
	}()
	for {
		if _, _, err := conn.ReadMessage(); err != nil {
			return
		}
	}
}

func (s *WatchServer) broadcastLive(rel string) {
	subs := s.live.snapshot(rel)
	if len(subs) == 0 {
		return
	}
	events, err := s.events(context.Background(), rel, s.pathVersion(rel))
	if err != nil {
		return
	}
	sess := Normalize(events, nil)
	setAgentPaths(sess, rel)

	prints := fingerprintAll(sess.Entries)
	prev := s.cache.get(rel)
	s.cache.set(rel, prints)

	var payload []byte
	if prev == nil || len(sess.Entries) < len(prev) {

		p, err := renderFullTimeline(sess)
		if err != nil {
			log.Printf("live render %s: %v", rel, err)
			return
		}
		payload = p
	} else {
		payload = buildLiveDiff(prev, sess)
	}

	if len(payload) == 0 {
		return
	}
	var dead []*websocket.Conn
	for _, c := range subs {
		if err := c.WriteMessage(websocket.TextMessage, payload); err != nil {
			dead = append(dead, c)
		}
	}
	for _, c := range dead {
		s.live.remove(rel, c)
		c.Close()
	}
}

func renderSingleEntry(e *Entry) []byte {
	var buf bytes.Buffer
	_ = tmpl.ExecuteTemplate(&buf, "entry", e)
	return buf.Bytes()
}

func buildLiveDiff(prev []entryPrint, sess *Session) []byte {
	entries := sess.Entries
	cur := fingerprintAll(entries)
	prevN := len(prev)
	curN := len(cur)

	var out bytes.Buffer

	limit := prevN
	if curN < limit {
		limit = curN
	}
	for i := 0; i < limit; i++ {
		if prev[i] == cur[i] {
			continue
		}
		rendered := renderSingleEntry(&entries[i])

		rendered = bytes.Replace(rendered,
			[]byte("<article "),
			[]byte(`<article hx-swap-oob="outerHTML" `), 1)
		out.Write(rendered)
	}

	if curN > prevN {
		out.WriteString(`<div hx-swap-oob="beforeend:#timeline-root">`)
		for i := prevN; i < curN; i++ {
			out.Write(renderSingleEntry(&entries[i]))
		}
		out.WriteString(`</div>`)
	}

	fmt.Fprintf(&out, `<span id="session-metadata" hx-swap-oob="outerHTML">%s</span>`,
		template.HTMLEscapeString(sess.Metadata))

	if out.Len() == 0 {
		return nil
	}
	return out.Bytes()
}

func renderFullTimeline(sess *Session) ([]byte, error) {
	var buf bytes.Buffer
	if err := tmpl.ExecuteTemplate(&buf, "timeline", newTimeline(sess, nil)); err != nil {
		return nil, fmt.Errorf("timeline: %w", err)
	}
	raw := buf.Bytes()
	anchor := []byte(`<div class="timeline" id="timeline-root">`)
	start := bytes.Index(raw, anchor)
	chipMarker := []byte(`<button class="scroll-chip"`)
	chip := bytes.Index(raw, chipMarker)
	if start < 0 || chip < 0 || chip <= start {
		return nil, fmt.Errorf("live fragment: could not locate timeline root in rendered output")
	}
	body := bytes.TrimRight(raw[start:chip], " \n\t")

	var out bytes.Buffer
	out.Write([]byte(`<div class="timeline" id="timeline-root" hx-swap-oob="outerHTML">`))
	out.Write(body[len(anchor):])
	if meta := sess.Metadata; meta != "" {
		fmt.Fprintf(&out, `<span id="session-metadata" hx-swap-oob="outerHTML">%s</span>`,
			template.HTMLEscapeString(meta))
	}
	return out.Bytes(), nil
}
