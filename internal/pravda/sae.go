package pravda

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"github.com/eordano/cccp/internal/sae"
)

const saeTagMinPct = 35

type SaeTag struct {
	Feature int
	Name    string
	Desc    string
	Stance  string
	Pct     int
	ID      string
	Encoder string
	Votes   sae.FeatureVotes
}

type saeSessionData struct {
	Encoder string
	Tagged  int
	JSON    string
	Payload *saePayload
}

const cookieSaeEncoder = "cw_sae_encoder"

func (s *WatchServer) chosenEncoder(ctx context.Context, r *http.Request) string {
	if s.Sae == nil {
		return ""
	}
	if e := r.URL.Query().Get("encoder"); e != "" && s.validEncoder(ctx, e) {
		return e
	}
	if c, err := r.Cookie(cookieSaeEncoder); err == nil && c.Value != "" && s.validEncoder(ctx, c.Value) {
		return c.Value
	}
	enc, _ := s.Sae.Current(ctx)
	return enc
}

func (s *WatchServer) decorateSae(ctx context.Context, sess *Session, enc string) *saeSessionData {
	if sess == nil {
		return nil
	}
	payload := s.saeData(ctx, enc, sess)
	data := &saeSessionData{Encoder: enc, Payload: payload}
	entryByIndex := make(map[int]*Entry, len(sess.Entries))
	for i := range sess.Entries {
		entryByIndex[sess.Entries[i].Index] = &sess.Entries[i]
	}
	featureByID := make(map[int]saeCatalogFeature, len(payload.Features))
	for _, f := range payload.Features {
		featureByID[f.ID] = f
	}
	for _, m := range payload.Messages {
		var tags []SaeTag
		for _, v := range m.Values {
			f := featureByID[v.Feature]
			if v.Value >= saeTagMinPct {
				tags = append(tags, SaeTag{Feature: v.Feature, Name: f.Label, Desc: f.Description, Pct: v.Value, ID: m.ID, Encoder: enc})
			}
		}
		if e := entryByIndex[m.Index]; e != nil {
			e.Sae = tags
		}
		if len(tags) > 0 {
			data.Tagged++
		}
	}
	data.JSON = payload.json()
	return data
}

func saeText(e *Entry) string {
	if e.Kind == KindUser || e.Kind == KindMessage {
		return e.Text
	}
	parts := []string{}
	add := func(value string) {
		if strings.TrimSpace(value) != "" {
			parts = append(parts, value)
		}
	}
	if e.Kind == KindTool {
		add(e.Name)
		var input any
		if json.Unmarshal(e.Input, &input) == nil && input != nil {
			if value, ok := input.(string); ok {
				add(value)
			} else if raw, err := json.Marshal(input); err == nil && string(raw) != "{}" {
				add(string(raw))
			}
		}
		for _, block := range e.ResultBlocks() {
			add(block.Text)
		}
	}
	add(e.Text)
	add(e.CommandMessage)
	add(e.CommandArgs)
	add(e.Summary)
	add(e.Result)
	add(e.Stderr)
	return strings.Join(parts, "\n")
}

func saeKind(e *Entry) string {
	if strings.TrimSpace(saeText(e)) == "" {
		return ""
	}
	if e.Kind == KindMessage {
		return "assistant"
	}
	return e.Kind
}

func tagsFor(acts []sae.Activation, feats map[int]sae.Feature, id string) []SaeTag {
	var tags []SaeTag
	for _, a := range acts {
		if a.Pct < saeTagMinPct {
			continue
		}
		f := feats[a.Feature]
		name := f.Name
		if name == "" {
			name = fmt.Sprintf("feature %d", a.Feature)
		}
		tags = append(tags, SaeTag{Feature: a.Feature, Name: name, Desc: f.Description, Stance: f.Stance, Pct: a.Pct, ID: id})
	}
	sort.Slice(tags, func(i, j int) bool { return tags[i].Pct > tags[j].Pct })
	return tags
}

type saeFeatureRow struct {
	sae.Feature
	Count int
	Share float64
	Votes sae.FeatureVotes
}

type saeFeaturesPage struct {
	Encoder string
	Model   string
	Layer   int
	Total   int
	Up      int
	Down    int
	Rows    []saeFeatureRow
	Sort    map[string]saeSortHeader
}

type saeIndexPage struct {
	Encoders []sae.Encoder
}

func (s *WatchServer) handleSaeIndex(w http.ResponseWriter, r *http.Request) {
	if s.Sae == nil {
		http.NotFound(w, r)
		return
	}
	encs, err := s.Sae.Encoders(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = tmpl.ExecuteTemplate(w, "sae-index-page", saeIndexPage{Encoders: encs})
}

func (s *WatchServer) handleSaeFeatures(w http.ResponseWriter, r *http.Request) {
	if s.Sae == nil {
		http.NotFound(w, r)
		return
	}
	ctx := r.Context()
	enc := r.URL.Query().Get("encoder")
	if enc == "" {
		var err error
		enc, err = s.Sae.Current(ctx)
		if err != nil || enc == "" {
			http.Error(w, "no SAE index imported", http.StatusNotFound)
			return
		}
	}
	feats, err := s.Sae.Features(ctx, enc)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	counts, total, err := s.Sae.FeatureCounts(ctx, enc)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	votes, up, down, err := s.Sae.FeatureVoteStats(ctx, enc)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	page := saeFeaturesPage{Encoder: enc, Total: total, Up: up, Down: down}
	if encs, err := s.Sae.EncoderCatalog(ctx); err == nil {
		for _, e := range encs {
			if e.Encoder == enc {
				page.Model, page.Layer = e.Model, e.Layer
			}
		}
	}
	for f, c := range counts {
		row := saeFeatureRow{Feature: feats[f], Count: c, Votes: votes[f]}
		row.Feature.Feature = f
		if total > 0 {
			row.Share = float64(c) * 100 / float64(total)
		}
		page.Rows = append(page.Rows, row)
	}
	order := readSaeSort(r.URL.Query(), "sort", saeFeatureSortKeys, "count")
	sortSaeRows(page.Rows, order, func(row saeFeatureRow) saeVoteKey {
		return saeVoteKey{ID: row.Feature.Feature, Value: row.Count, Votes: row.Votes}
	})
	u := *r.URL
	q := u.Query()
	q.Set("encoder", enc)
	u.RawQuery = q.Encode()
	page.Sort = saeSortHeaders(&u, "sort", order, saeFeatureSortKeys)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = tmpl.ExecuteTemplate(w, "sae-features-page", page)
}

type saeSimilarPage struct {
	Encoder string
	Target  *sae.Record
	Tags    []SaeTag
	Kind    string
	Matches []saeMatchRow
}

type saeMatchRow struct {
	sae.Match
	Tags []SaeTag
	Pct  int
}

func (s *WatchServer) handleSaeSimilar(w http.ResponseWriter, r *http.Request) {
	if s.Sae == nil {
		http.NotFound(w, r)
		return
	}
	ctx := r.Context()
	enc := s.chosenEncoder(ctx, r)
	if enc == "" {
		http.Error(w, "no SAE index imported", http.StatusNotFound)
		return
	}
	id := r.PathValue("id")
	target, err := s.Sae.Get(ctx, enc, id)
	if err != nil || target == nil {
		http.NotFound(w, r)
		return
	}
	feats, err := s.Sae.Features(ctx, enc)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("n"))
	if limit <= 0 {
		limit = sae.SimilarDefaultLimit
	}
	kind := r.URL.Query().Get("kind")
	matches, err := s.Sae.Similar(ctx, enc, target.Features, kind, limit+1)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	page := saeSimilarPage{Encoder: enc, Target: target, Tags: tagsFor(target.Features, feats, id), Kind: kind}
	for i := range page.Tags {
		page.Tags[i].Encoder = enc
	}
	for _, m := range matches {
		if m.ID == id {
			continue
		}
		page.Matches = append(page.Matches, saeMatchRow{Match: m, Tags: tagsFor(m.Features, feats, m.ID), Pct: int(m.Score*100 + 0.5)})
	}
	for i := range page.Matches {
		for j := range page.Matches[i].Tags {
			page.Matches[i].Tags[j].Encoder = enc
		}
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = tmpl.ExecuteTemplate(w, "sae-similar-page", page)
}

type saeInspectorData struct {
	JSON        string
	Index       int
	Dictionary  string
	Detail      string
	SettingsURL string
	Tags        []SaeTag
	Sort        map[string]saeSortHeader
	NoVotes     bool
}

func saeInspector(data *saeSessionData, entry *Entry) *saeInspectorData {
	view := &saeInspectorData{Index: entry.Index, Detail: "This event has no readable text to analyze with SAE.", SettingsURL: "/sae/settings"}
	if data == nil || data.Payload == nil {
		return view
	}
	payload := *data.Payload
	view.Dictionary = payload.Dictionary.Name
	view.SettingsURL = payload.SettingsURL
	for _, message := range payload.Messages {
		if message.Index != entry.Index {
			continue
		}
		payload.Messages = []saeMessage{message}
		payload.Features = append([]saeCatalogFeature(nil), payload.Features...)
		for i := range payload.Features {
			payload.Features[i].Count = 0
			for _, value := range message.Values {
				if value.Feature == payload.Features[i].ID && value.Value > 0 {
					payload.Features[i].Count = 1
					break
				}
			}
		}
		if payload.RefreshURL != "" {
			u, _ := url.Parse(payload.RefreshURL)
			q := u.Query()
			q.Set("event", strconv.Itoa(entry.Index))
			u.RawQuery = q.Encode()
			payload.RefreshURL = u.String()
		}
		payload.Status.Eligible = 1
		summarizeSaeStatus(&payload)
		view.Detail = message.Detail
		if message.Processed {
			view.Detail = "All saved scaled activations are shown, including values below the display threshold. Percentages are not confidence scores."
			if len(message.Values) == 0 {
				view.Detail = "A saved result exists with no activations."
			}
		}
		for _, value := range message.Values {
			name := fmt.Sprintf("Feature %d", value.Feature)
			desc := ""
			for _, feature := range payload.Features {
				if feature.ID == value.Feature {
					name = feature.Label
					desc = feature.Description
					break
				}
			}
			view.Tags = append(view.Tags, SaeTag{Feature: value.Feature, Name: name, Desc: desc, Pct: value.Value, ID: message.ID, Encoder: data.Encoder})
		}
		sort.Slice(view.Tags, func(i, j int) bool { return view.Tags[i].Pct > view.Tags[j].Pct })
		view.JSON = payload.json()
		return view
	}
	return view
}
