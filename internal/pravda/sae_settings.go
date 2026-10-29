package pravda

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"time"

	"github.com/eordano/cccp/internal/sae"
)

type saeDictionary struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Features  int    `json:"features"`
	CreatedAt string `json:"created_at"`
	Records   *int   `json:"records"`
	Current   bool   `json:"current"`
	TopK      int    `json:"top_k"`
	Layer     int    `json:"layer"`
	Model     string `json:"model"`
}
type saeCatalogFeature struct {
	ID          int    `json:"id"`
	Label       string `json:"label"`
	Description string `json:"description"`
	Count       int    `json:"count"`
}
type saeValue struct {
	Feature int `json:"feature"`
	Value   int `json:"value"`
}
type saeExecutor interface {
	Ensure(context.Context, string, sae.Record, string) (sae.JobState, error)
	States(context.Context, string, []string) (map[string]sae.JobState, error)
	Supports(string) bool
}

type saeMessage struct {
	Bytes     int        `json:"-"`
	Retrying  bool       `json:"retrying,omitempty"`
	State     string     `json:"state"`
	Detail    string     `json:"detail,omitempty"`
	ID        string     `json:"id"`
	Index     int        `json:"index"`
	Label     string     `json:"label"`
	Kind      string     `json:"kind"`
	Preview   string     `json:"preview"`
	Processed bool       `json:"processed"`
	Values    []saeValue `json:"values"`
}
type saeStatus struct {
	Saved         int    `json:"saved"`
	Queued        int    `json:"queued"`
	Running       int    `json:"running"`
	AwaitingRetry int    `json:"awaiting_retry"`
	NotRequested  int    `json:"not_requested"`
	Unavailable   int    `json:"unavailable"`
	Poll          bool   `json:"poll"`
	Active        int    `json:"active"`
	Failed        int    `json:"failed"`
	State         string `json:"state"`
	Tagged        int    `json:"tagged"`
	Eligible      int    `json:"eligible"`
	Records       *int   `json:"records"`
	UpdatedAt     string `json:"updated_at"`
	Detail        string `json:"detail"`
}
type saePayload struct {
	Dictionary   saeDictionary       `json:"dictionary"`
	Dictionaries []saeDictionary     `json:"dictionaries"`
	Features     []saeCatalogFeature `json:"features"`
	Messages     []saeMessage        `json:"messages"`
	Status       saeStatus           `json:"status"`
	Session      string              `json:"session"`
	SettingsURL  string              `json:"settings_url"`
	FeaturesURL  string              `json:"features_url"`
	RefreshURL   string              `json:"refresh_url"`
}

func (p *saePayload) json() string { b, _ := json.Marshal(p); return string(b) }
func (s *WatchServer) validEncoder(ctx context.Context, enc string) bool {
	if s.Sae == nil {
		return false
	}
	ok, err := s.Sae.HasEncoder(ctx, enc)
	return err == nil && ok
}
func (s *WatchServer) saeData(ctx context.Context, enc string, sess *Session) *saePayload {
	return s.saeDataWithCounts(ctx, enc, sess, sess == nil)
}

func (s *WatchServer) saeDataWithCounts(ctx context.Context, enc string, sess *Session, withCounts bool) *saePayload {
	p := &saePayload{Dictionaries: []saeDictionary{}, Features: []saeCatalogFeature{}, Messages: []saeMessage{}, SettingsURL: "/sae/settings", FeaturesURL: "/sae/features?encoder=" + url.QueryEscape(enc)}
	defer summarizeSaeStatus(p)
	p.Status = saeStatus{State: "unavailable", Detail: "No SAE dictionary is available. Processing activity is not reported by the imported index."}
	ids := []string{}
	if sess != nil {
		for _, e := range sess.Entries {
			if k := saeKind(&e); k != "" {
				text := saeText(&e)
				id := sae.RecordID(k, text)
				ids = append(ids, id)
				preview := []rune(text)
				if len(preview) > 240 {
					preview = preview[:240]
				}
				p.Messages = append(p.Messages, saeMessage{Bytes: len(text), ID: id, Index: e.Index, Label: fmt.Sprintf("Event %d · %s", e.Index, k), Kind: k, Preview: string(preview), Values: []saeValue{}})
			}
		}
		p.Status.Eligible = len(p.Messages)
	}
	if s.Sae == nil {
		return p
	}
	var encs []sae.Encoder
	var err error
	if withCounts {
		encs, err = s.Sae.Encoders(ctx)
	} else {
		encs, err = s.Sae.EncoderCatalog(ctx)
	}
	if err != nil {
		p.Status.State = "error"
		p.Status.Detail = "The SAE index could not be read."
		return p
	}
	for _, e := range encs {
		name := e.Label
		if name == "" {
			name = "weekly"
		}
		d := saeDictionary{ID: e.Encoder, Name: name, Features: e.Width, CreatedAt: e.ImportedAt.UTC().Format(time.RFC3339), Current: e.Current, TopK: e.TopK, Layer: e.Layer, Model: e.Model}
		if e.Records >= 0 {
			n := e.Records
			d.Records = &n
		}
		p.Dictionaries = append(p.Dictionaries, d)
		if e.Encoder == enc {
			p.Dictionary = d
		}
	}
	if p.Dictionary.ID == "" {
		return p
	}
	p.SettingsURL += "?encoder=" + url.QueryEscape(enc)
	p.Status.Records = p.Dictionary.Records
	p.Status.UpdatedAt = p.Dictionary.CreatedAt
	feats, err := s.Sae.Features(ctx, enc)
	if err != nil {
		p.Status.State = "error"
		p.Status.Detail = "Feature labels could not be read."
		return p
	}
	counts := map[int]int{}
	if sess == nil {
		counts, _, err = s.Sae.FeatureCounts(ctx, enc)
	} else {
		var found map[string]sae.Record
		found, err = s.Sae.ByIDs(ctx, enc, ids)
		if err == nil {
			for i := range p.Messages {
				m := &p.Messages[i]
				rec, ok := found[m.ID]
				m.Processed = ok
				if ok {
					m.State = "complete"
				}
				if !ok {
					continue
				}
				p.Status.Tagged++
				for _, a := range rec.Features {
					m.Values = append(m.Values, saeValue{Feature: a.Feature, Value: a.Pct})
					if a.Pct > 0 {
						counts[a.Feature]++
					}
				}
				sort.Slice(m.Values, func(i, j int) bool { return m.Values[i].Value > m.Values[j].Value })
			}
		}
	}
	if err != nil {
		p.Status.State = "error"
		p.Status.Detail = "Stored activations could not be read."
		return p
	}
	for id := 0; id < p.Dictionary.Features; id++ {
		if _, ok := feats[id]; !ok {
			feats[id] = sae.Feature{Feature: id}
		}
	}
	for id := range counts {
		if _, ok := feats[id]; !ok {
			feats[id] = sae.Feature{Feature: id}
		}
	}
	for id, f := range feats {
		name := f.Name
		if name == "" {
			name = fmt.Sprintf("Feature %d", id)
		}
		p.Features = append(p.Features, saeCatalogFeature{ID: id, Label: name, Description: f.Description, Count: counts[id]})
	}
	sort.Slice(p.Features, func(i, j int) bool {
		if p.Features[i].Count == p.Features[j].Count {
			return p.Features[i].ID < p.Features[j].ID
		}
		return p.Features[i].Count > p.Features[j].Count
	})
	p.Status.State = "ready"
	p.Status.Detail = "Values are stored for this dictionary and reused across requests. Updated time is the dictionary import time."
	if sess == nil {
		p.Status.Detail = "Dictionary available. Record count covers the imported index; global eligible-message coverage and processing activity are not reported."
	} else if p.Status.Tagged < p.Status.Eligible {
		p.Status.State = "partial"
		p.Status.Detail = "Some eligible messages have no stored result for this dictionary. Open a message to see scoring availability."
	}
	s.applySaeJobs(ctx, p, ids)
	return p
}

type saeSettingsPage struct{ JSON string }

func (s *WatchServer) handleSaeSettings(w http.ResponseWriter, r *http.Request) {
	var sess *Session
	if rel := r.URL.Query().Get("session"); rel != "" {
		events, err := s.events(r.Context(), rel, s.pathVersion(rel))
		if err != nil {
			sessionError(w, r, err)
			return
		}
		sess = Normalize(events, nil)
	}
	p := s.saeDataWithCounts(r.Context(), s.chosenEncoder(r.Context(), r), sess, true)
	p.Session = r.URL.Query().Get("session")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = tmpl.ExecuteTemplate(w, "sae-settings-page", saeSettingsPage{JSON: p.json()})
}
func setSaeSession(data *saeSessionData, path string) {
	if data == nil {
		return
	}
	data.Payload.Session = path
	data.Payload.RefreshURL = (&url.URL{Path: "/sae/session/" + path, RawQuery: url.Values{"encoder": {data.Encoder}}.Encode()}).String()
	data.Payload.SettingsURL = saeSessionSettingsURL(data.Payload.SettingsURL, path)
	data.JSON = data.Payload.json()
}
func (s *WatchServer) handleSaeSession(w http.ResponseWriter, r *http.Request) {
	rel := r.PathValue("path")
	ctx := r.Context()
	ver := s.pathVersion(rel)
	sess, ok := s.parsed.getCachedSession(rel, ver)
	if !ok {
		events, err := s.events(ctx, rel, ver)
		if err != nil {
			sessionError(w, r, err)
			return
		}
		sess = Normalize(events, nil)
	}
	encoder := s.chosenEncoder(ctx, r)
	transient := s.enqueueSae(ctx, r, rel, sess, encoder)
	saeSession := sess
	if event := r.URL.Query().Get("event"); event != "" {
		index, err := strconv.Atoi(event)
		if err != nil || index < 1 || index > len(sess.Entries) {
			http.NotFound(w, r)
			return
		}
		saeSession = &Session{Entries: []Entry{sess.Entries[index-1]}, Facts: sess.Facts}
	}
	p := s.saeData(ctx, encoder, saeSession)
	applySaeTransient(p, transient)
	p.Session = rel
	p.RefreshURL = (&url.URL{Path: "/sae/session/" + rel, RawQuery: url.Values{"encoder": {p.Dictionary.ID}}.Encode()}).String()
	p.SettingsURL = saeSessionSettingsURL(p.SettingsURL, rel)
	if limit := r.URL.Query().Get("limit"); limit != "" {
		u, _ := url.Parse(p.RefreshURL)
		q := u.Query()
		q.Set("limit", limit)
		u.RawQuery = q.Encode()
		p.RefreshURL = u.String()
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	if event := r.URL.Query().Get("event"); event != "" {
		index, err := strconv.Atoi(event)
		if err != nil || index < 1 || index > len(sess.Entries) {
			http.NotFound(w, r)
			return
		}
		view := saeInspector(&saeSessionData{Encoder: encoder, Payload: p}, &sess.Entries[index-1])
		if view.JSON != "" {
			_, _ = w.Write([]byte(view.JSON))
			return
		}
	}
	_ = json.NewEncoder(w).Encode(p)
}

func saeSessionSettingsURL(base, path string) string {
	u, _ := url.Parse(base)
	q := u.Query()
	q.Set("session", path)
	u.RawQuery = q.Encode()
	return u.String()
}

func (s *WatchServer) enqueueSae(ctx context.Context, r *http.Request, rel string, sess *Session, enc string) map[string]sae.JobState {
	if s.SaeScorer == nil || s.Sae == nil || !s.SaeScorer.Supports(enc) {
		return nil
	}
	var entries []*Entry
	if event := r.URL.Query().Get("event"); event != "" {
		index, err := strconv.Atoi(event)
		if err != nil || index < 1 || index > len(sess.Entries) {
			return nil
		}
		entries = []*Entry{&sess.Entries[index-1]}
	} else {
		for i := range sess.Entries {
			entries = append(entries, &sess.Entries[i])
		}
	}
	ids := []string{}
	for _, entry := range entries {
		if kind := saeKind(entry); kind != "" {
			ids = append(ids, sae.RecordID(kind, saeText(entry)))
		}
	}
	found, err := s.Sae.ByIDs(ctx, enc, ids)
	if err != nil {
		return nil
	}
	jobs, err := s.SaeScorer.States(ctx, enc, ids)
	if err != nil {
		return nil
	}
	transient := map[string]sae.JobState{}
	queued := 0
	for i := len(entries) - 1; i >= 0 && queued < 32; i-- {
		entry := entries[i]
		kind := saeKind(entry)
		text := saeText(entry)
		if kind == "" || len(text) > sae.MaxCaptureBytes {
			continue
		}
		id := sae.RecordID(kind, text)
		if _, ok := found[id]; ok {
			continue
		}
		if job, ok := jobs[id]; ok && (job.State == "pending" || job.State == "running" || job.State == "failed") {
			continue
		}
		snippet := []rune(text)
		if len(snippet) > 240 {
			snippet = snippet[:240]
		}
		record := sae.Record{ID: id, Kind: kind, Agent: entry.Agent, Path: rel, Line: entry.Index, Snippet: string(snippet), Time: entry.TS}
		if sess.Facts != nil {
			record.Session = sess.Facts.SessionID
		}
		state, err := s.SaeScorer.Ensure(ctx, enc, record, text)
		if err != nil {
			transient[id] = sae.JobState{State: "unavailable", Error: "SAE scoring could not be queued. Refresh to try again."}
			break
		}
		jobs[id] = state
		if state.State == "unavailable" {
			transient[id] = state
		}
		queued++
	}
	return transient
}

func (s *WatchServer) applySaeJobs(ctx context.Context, p *saePayload, ids []string) {
	supported := s.SaeScorer != nil && s.SaeScorer.Supports(p.Dictionary.ID)
	jobs := map[string]sae.JobState{}
	if supported {
		var err error
		jobs, err = s.SaeScorer.States(ctx, p.Dictionary.ID, ids)
		if err != nil {
			p.Status.State = "error"
			p.Status.Detail = "SAE execution status could not be read."
			return
		}
	}
	for i := range p.Messages {
		message := &p.Messages[i]
		if message.Processed {
			continue
		}
		message.State = "unavailable"
		message.Detail = "No stored result is available. On-demand scoring is not configured for this dictionary."
		if !supported {
			continue
		}
		if message.Bytes > sae.MaxCaptureBytes {
			message.Detail = "This message exceeds the 1 MiB on-demand capture size limit."
			continue
		}
		message.State = "missing"
		message.Detail = "No stored result. This event has not been requested yet; it will be queued in a following batch."
		job, ok := jobs[message.ID]
		if !ok {
			continue
		}
		message.State = job.State
		switch job.State {
		case "pending":
			message.Detail = "Queued for SAE scoring. The result will be saved."
			p.Status.Active++
		case "running":
			message.Detail = "SAE scoring is running. The result will be saved."
			p.Status.Active++
		case "failed":
			message.Detail = job.Error
			p.Status.Failed++
			if !job.RetryAt.IsZero() {
				message.Retrying = true
				p.Status.Active++
				message.Detail += " Automatic retry scheduled for " + job.RetryAt.UTC().Format(time.RFC3339) + "."
			}
		}
	}
	summarizeSaeStatus(p)
}

func applySaeTransient(p *saePayload, states map[string]sae.JobState) {
	for i := range p.Messages {
		message := &p.Messages[i]
		if state, ok := states[message.ID]; ok && !message.Processed {
			message.State = state.State
			message.Detail = state.Error
			message.Retrying = !state.RetryAt.IsZero()
		}
	}
	summarizeSaeStatus(p)
}

func summarizeSaeStatus(p *saePayload) {
	hadError := p.Status.State == "error"
	status := &p.Status
	status.Saved = 0
	status.Queued = 0
	status.Running = 0
	status.AwaitingRetry = 0
	status.Failed = 0
	status.NotRequested = 0
	status.Unavailable = 0
	for _, message := range p.Messages {
		switch {
		case message.Processed:
			status.Saved++
		case message.Retrying:
			status.AwaitingRetry++
		case message.State == "pending":
			status.Queued++
		case message.State == "running":
			status.Running++
		case message.State == "failed":
			status.Failed++
		case message.State == "unavailable" || message.State == "":
			status.Unavailable++
		default:
			status.NotRequested++
		}
	}
	status.Tagged = status.Saved
	status.Active = status.Queued + status.Running + status.AwaitingRetry
	status.Poll = status.Active > 0 || status.NotRequested > 0
	if hadError {
		status.Poll = false
		return
	}
	if len(p.Messages) == 0 {
		return
	}
	status.State = "partial"
	switch {
	case status.Poll:
		status.State = "processing"
	case status.Saved == status.Eligible:
		status.State = "ready"
	case status.Failed > 0:
		status.State = "failed"
	case status.Unavailable == status.Eligible:
		status.State = "unavailable"
	}
	status.Detail = fmt.Sprintf("%d saved · %d queued · %d running · %d awaiting retry · %d failed · %d not requested · %d unavailable", status.Saved, status.Queued, status.Running, status.AwaitingRetry, status.Failed, status.NotRequested, status.Unavailable)
}
