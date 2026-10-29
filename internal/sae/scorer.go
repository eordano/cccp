package sae

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"
)

// cpuEncoder follows nv-sae normalize -> encoder matrix -> bias -> top-k ReLU.
// Scale always comes from the original dictionary scoring run, never this request.
type cpuEncoder struct {
	matrix, bias, mu, sd, scale []float32
	hidden, width, k            int
}

func safeEncoderID(id string) bool {
	b, err := hex.DecodeString(id)
	return err == nil && len(b) == sha256.Size && strings.ToLower(id) == id
}
func scalePath(dir string) string {
	p := filepath.Join(dir, "scale.safetensors")
	if _, err := os.Stat(p); err == nil {
		return p
	}
	return filepath.Join(dir, "scored.safetensors")
}
func loadCPUEncoder(root, id string) (*cpuEncoder, error) {
	if !safeEncoderID(id) {
		return nil, errors.New("invalid dictionary identifier")
	}
	dir := filepath.Join(root, id)
	file := filepath.Join(dir, "sae.safetensors")
	raw, err := os.ReadFile(file)
	if err != nil {
		return nil, errors.New("dictionary weights unavailable")
	}
	sum := sha256.Sum256(raw)
	if hex.EncodeToString(sum[:]) != id {
		return nil, errors.New("dictionary weight checksum mismatch")
	}
	st, err := readSafetensors(file)
	if err != nil {
		return nil, errors.New("invalid dictionary tensors")
	}
	m := &cpuEncoder{}
	var shape []int
	m.matrix, shape, err = st.f32("encoder")
	if err != nil || len(shape) != 2 || shape[0] < 1 || shape[1] < 1 || len(m.matrix) != shape[0]*shape[1] {
		return nil, errors.New("invalid encoder dimensions")
	}
	m.hidden, m.width = shape[0], shape[1]
	for _, item := range []struct {
		name string
		dst  *[]float32
		size int
	}{{"encoder_bias", &m.bias, m.width}, {"mu", &m.mu, m.hidden}, {"sd", &m.sd, m.hidden}} {
		values, _, err := st.f32(item.name)
		if err != nil || len(values) != item.size {
			return nil, errors.New("invalid dictionary normalization")
		}
		*item.dst = values
	}
	top, _, err := st.f32("top_k")
	if err != nil || len(top) != 1 || top[0] < 1 || top[0] > float32(m.width) || float32(int(top[0])) != top[0] {
		return nil, errors.New("invalid dictionary top-k")
	}
	m.k = int(top[0])
	scales, err := readSafetensors(scalePath(dir))
	if err != nil {
		return nil, errors.New("original dictionary scales unavailable")
	}
	m.scale, _, err = scales.f32("scale")
	if err != nil || len(m.scale) != m.width {
		return nil, errors.New("invalid dictionary scales")
	}
	for _, values := range [][]float32{m.matrix, m.bias, m.mu, m.sd, m.scale} {
		for _, v := range values {
			if math.IsNaN(float64(v)) || math.IsInf(float64(v), 0) {
				return nil, errors.New("non-finite dictionary value")
			}
		}
	}
	for _, v := range m.sd {
		if v <= 0 {
			return nil, errors.New("invalid dictionary deviation")
		}
	}
	for _, v := range m.scale {
		if v <= 0 {
			return nil, errors.New("invalid dictionary scale")
		}
	}
	return m, nil
}
func (m *cpuEncoder) score(mean []float32) ([]Activation, error) {
	if len(mean) != m.hidden {
		return nil, errors.New("capture dimension does not match dictionary")
	}
	pre := make([]float32, m.width)
	for i, x := range mean {
		if math.IsNaN(float64(x)) || math.IsInf(float64(x), 0) {
			return nil, errors.New("non-finite capture value")
		}
		z := (x - m.mu[i]) / m.sd[i]
		for f := 0; f < m.width; f++ {
			pre[f] += z * m.matrix[i*m.width+f]
		}
	}
	for i := range pre {
		pre[i] += m.bias[i]
	}
	indices := make([]int, m.width)
	for i := range indices {
		indices[i] = i
	}
	sort.SliceStable(indices, func(i, j int) bool { return pre[indices[i]] > pre[indices[j]] })
	acts := []Activation{}
	for _, f := range indices[:m.k] {
		if pre[f] <= 0 {
			continue
		}
		v := float64(pre[f]) / float64(m.scale[f]) * 100
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return nil, errors.New("non-finite scored activation")
		}
		pct := int(math.Round(math.Min(100, v)))
		acts = append(acts, Activation{Feature: f, Pct: pct})
	}
	return acts, nil
}

// MaxCaptureBytes bounds durable job size; long inputs are split without truncation.
const MaxCaptureBytes = 1 << 20

type captureHTTPError struct {
	Status  int
	Message string
}

func (e *captureHTTPError) Error() string {
	return fmt.Sprintf("capture service returned HTTP %d", e.Status)
}

type capturedMean struct {
	Mean   []float32 `json:"mean"`
	Tokens int       `json:"tokens"`
	Parts  int       `json:"-"`
}

type captureTarget struct {
	Encoder string
	Model   string
	Layer   int
	Hidden  int
}

func (t captureTarget) describe() string {
	return fmt.Sprintf("Dictionary %.12s belongs to %s layer %d", t.Encoder, t.Model, t.Layer)
}

type captureMismatchError struct{ msg string }

func (e *captureMismatchError) Error() string { return e.msg }

func cleanModelID(id string) string {
	if id == "" || len(id) > 128 {
		return "an unnamed model"
	}
	for _, c := range id {
		ok := c == '.' || c == '_' || c == '-' || c == '/' || c == ':' || c == '@' || c == '+' || (c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
		if !ok {
			return "an unnamed model"
		}
	}
	return id
}

func (o *OnDemand) capture(ctx context.Context, text string, t captureTarget) ([]float32, error) {
	if len(text) > MaxCaptureBytes || !utf8.ValidString(text) {
		return nil, errors.New("invalid capture text")
	}
	result, err := o.captureText(ctx, text, t)
	if err == nil && result.Parts > 1 {
		slog.Info("SAE capture chunked", "bytes", len(text), "chunks", result.Parts, "tokens", result.Tokens)
	}
	return result.Mean, err
}

func (o *OnDemand) captureText(ctx context.Context, text string, t captureTarget) (capturedMean, error) {
	result, err := o.capturePart(ctx, text, t)
	var failure *captureHTTPError
	if !errors.As(err, &failure) || failure.Status != http.StatusBadRequest || failure.Message != "context limit" {
		return result, err
	}
	mid := len(text) / 2
	for mid < len(text) && !utf8.RuneStart(text[mid]) {
		mid++
	}
	if mid == 0 || mid == len(text) {
		return capturedMean{}, err
	}
	left, err := o.captureText(ctx, text[:mid], t)
	if err != nil {
		return capturedMean{}, err
	}
	right, err := o.captureText(ctx, text[mid:], t)
	if err != nil {
		return capturedMean{}, err
	}
	if len(left.Mean) != len(right.Mean) {
		return capturedMean{}, errors.New("invalid capture response")
	}
	total := left.Tokens + right.Tokens
	for i := range left.Mean {
		left.Mean[i] = float32((float64(left.Mean[i])*float64(left.Tokens) + float64(right.Mean[i])*float64(right.Tokens)) / float64(total))
	}
	left.Tokens = total
	left.Parts += right.Parts
	return left, nil
}

func (o *OnDemand) servedModels(ctx context.Context) string {
	req, err := http.NewRequestWithContext(ctx, "GET", strings.TrimRight(o.cfg.CaptureURL, "/")+"/v1/capture", nil)
	if err != nil {
		return ""
	}
	resp, err := o.client.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	var body struct {
		Models []struct {
			Model   string `json:"model"`
			Capture bool   `json:"capture"`
		} `json:"models"`
	}
	if resp.StatusCode != http.StatusOK || json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&body) != nil {
		return ""
	}
	var ids []string
	for _, m := range body.Models {
		if m.Capture && len(ids) < 8 {
			ids = append(ids, cleanModelID(m.Model))
		}
	}
	return strings.Join(ids, ", ")
}

func (o *OnDemand) postCapture(ctx context.Context, route string, body []byte) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, "POST", strings.TrimRight(o.cfg.CaptureURL, "/")+route, bytes.NewReader(body))
	if err != nil {
		return nil, errors.New("invalid capture endpoint")
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := o.client.Do(req)
	if err != nil {
		return nil, errors.New("capture service unavailable")
	}
	return resp, nil
}

type captureFailure struct {
	err          error
	routeMissing bool
}

func (o *OnDemand) captureFailure(ctx context.Context, resp *http.Response, t captureTarget) captureFailure {
	var body struct {
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	_ = json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&body)
	switch {
	case resp.StatusCode == http.StatusNotFound && body.Error.Type == "model_not_found":
		msg := t.describe() + "; the capture engine does not serve that model"
		if served := o.servedModels(ctx); served != "" {
			msg += " (it serves " + served + ")"
		}
		return captureFailure{err: &captureMismatchError{msg + "."}}
	case resp.StatusCode == http.StatusNotImplemented:
		return captureFailure{err: &captureMismatchError{t.describe() + "; the capture engine serving it cannot capture layers (HTTP 501)."}}
	}
	out := &captureHTTPError{Status: resp.StatusCode}
	if resp.StatusCode == http.StatusBadRequest && body.Error.Message == "each input must hold 1..=2048 tokens" {
		out.Message = "context limit"
	}
	return captureFailure{err: out, routeMissing: resp.StatusCode == http.StatusNotFound && body.Error.Type == ""}
}

func (o *OnDemand) capturePart(ctx context.Context, text string, t captureTarget) (capturedMean, error) {
	body, _ := json.Marshal(struct {
		Model      string   `json:"model"`
		Layer      int      `json:"layer"`
		Texts      []string `json:"texts"`
		Tokens     bool     `json:"tokens"`
		MaxContext int      `json:"max_context"`
	}{t.Model, t.Layer, []string{text}, false, 2048})
	var resp *http.Response
	for _, route := range []string{"/v1/capture", "/v1/laguna/capture"} {
		var err error
		if resp, err = o.postCapture(ctx, route, body); err != nil {
			return capturedMean{}, err
		}
		if resp.StatusCode == http.StatusOK {
			break
		}
		failure := o.captureFailure(ctx, resp, t)
		resp.Body.Close()
		if !failure.routeMissing || route != "/v1/capture" {
			return capturedMean{}, failure.err
		}
	}
	defer resp.Body.Close()
	var result struct {
		Model   string         `json:"model"`
		Layer   int            `json:"layer"`
		Hidden  int            `json:"hidden_size"`
		Records []capturedMean `json:"records"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&result); err != nil || result.Layer != t.Layer || len(result.Records) != 1 || result.Hidden < 1 || result.Hidden != len(result.Records[0].Mean) || result.Records[0].Tokens < 1 {
		return capturedMean{}, errors.New("invalid capture response")
	}
	if result.Model != t.Model {
		return capturedMean{}, &captureMismatchError{fmt.Sprintf("%s; the capture engine answered with %s.", t.describe(), cleanModelID(result.Model))}
	}
	if result.Hidden != t.Hidden {
		return capturedMean{}, &captureMismatchError{fmt.Sprintf("%s with hidden size %d; the capture engine returned hidden size %d.", t.describe(), t.Hidden, result.Hidden)}
	}
	for _, v := range result.Records[0].Mean {
		if math.IsNaN(float64(v)) || math.IsInf(float64(v), 0) {
			return capturedMean{}, errors.New("invalid capture response")
		}
	}
	result.Records[0].Parts = 1
	return result.Records[0], nil
}
