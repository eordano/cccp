package sae

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"strings"
	"testing"
	"unicode/utf8"
)

var testTarget = captureTarget{Encoder: "0123456789abcdef", Model: "vendor/test-model", Layer: 19, Hidden: 2}

func TestCaptureSplitsWithoutLosingTextAndWeightsByTokens(t *testing.T) {
	for _, text := range []string{strings.Repeat("你好 🌎 café\n", 25), strings.Repeat("abc def\n", 9000) + "FINAL TEXT"} {
		t.Run(string([]rune(text)[:3]), func(t *testing.T) {
			var accepted strings.Builder
			var total, weighted float64
			chunks := 0
			o, _, _ := testOnDemand(t, func(w http.ResponseWriter, r *http.Request) {
				var req struct {
					Texts      []string `json:"texts"`
					MaxContext int      `json:"max_context"`
				}
				if err := json.NewDecoder(r.Body).Decode(&req); err != nil || len(req.Texts) != 1 || req.MaxContext != 2048 {
					t.Fatal("invalid capture request")
				}
				part := req.Texts[0]
				if !utf8.ValidString(part) {
					t.Fatal("split broke UTF-8")
				}
				if len(part) > 1000 || (len(text) < 1000 && len(part) > 40) {
					w.WriteHeader(400)
					w.Write([]byte(`{"error":{"message":"each input must hold 1..=2048 tokens"}}`))
					return
				}
				chunks++
				accepted.WriteString(part)
				tokens := len([]rune(part)) + chunks
				value := float64(chunks)
				weighted += value * float64(tokens)
				total += float64(tokens)
				json.NewEncoder(w).Encode(map[string]any{"model": "vendor/test-model", "layer": 19, "hidden_size": 2, "records": []map[string]any{{"tokens": tokens, "mean": []float64{value, 2 * value}}}})
			})
			mean, err := o.capture(context.Background(), text, testTarget)
			if err != nil {
				t.Fatal(err)
			}
			if chunks < 2 || accepted.String() != text {
				t.Fatal("capture omitted or duplicated input")
			}
			if math.Abs(float64(mean[0])-weighted/total) > 0.0001 || math.Abs(float64(mean[1])-2*weighted/total) > 0.0002 {
				t.Fatalf("incorrect token weighting: %v", mean)
			}
		})
	}
}

func TestCaptureDoesNotSplitOtherClientErrors(t *testing.T) {
	calls := 0
	o, _, _ := testOnDemand(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(400)
		w.Write([]byte(`{"error":{"message":"some private diagnostic"}}`))
	})
	_, err := o.capture(context.Background(), "a valid message", testTarget)
	var status *captureHTTPError
	if !errors.As(err, &status) || status.Status != 400 || calls != 1 || strings.Contains(err.Error(), "private") {
		t.Fatalf("error must remain sanitized and not cause splitting: %v %d", err, calls)
	}
}

func TestCaptureDiscardsPartialSuccess(t *testing.T) {
	calls := 0
	o, _, _ := testOnDemand(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.WriteHeader(400)
			w.Write([]byte(`{"error":{"message":"each input must hold 1..=2048 tokens"}}`))
			return
		}
		if calls == 2 {
			captureOK(w, r)
			return
		}
		w.WriteHeader(503)
	})
	mean, err := o.capture(context.Background(), "a valid long message", testTarget)
	if err == nil || len(mean) != 0 || calls != 3 {
		t.Fatalf("partial capture returned as success: %v %v %d", mean, err, calls)
	}
}

func TestCaptureRefusesModelAndHiddenMismatch(t *testing.T) {
	for _, tc := range []struct {
		name, body, want string
	}{
		{"model", `{"model":"other/model","layer":19,"hidden_size":2,"records":[{"tokens":2,"mean":[1,2]}]}`, "Dictionary 0123456789ab belongs to vendor/test-model layer 19; the capture engine answered with other/model."},
		{"hidden", `{"model":"vendor/test-model","layer":19,"hidden_size":3,"records":[{"tokens":2,"mean":[1,2,3]}]}`, "Dictionary 0123456789ab belongs to vendor/test-model layer 19 with hidden size 2; the capture engine returned hidden size 3."},
		{"hostile", `{"model":"<script>","layer":19,"hidden_size":2,"records":[{"tokens":2,"mean":[1,2]}]}`, "Dictionary 0123456789ab belongs to vendor/test-model layer 19; the capture engine answered with an unnamed model."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o, _, _ := testOnDemand(t, func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(tc.body)) })
			_, err := o.capture(context.Background(), "text", testTarget)
			message, retry := scoringFailure(err)
			if message != tc.want || retry {
				t.Fatalf("got %q retry=%v", message, retry)
			}
		})
	}
}

func TestCaptureExplainsUnservedModelAndUnsupportedEngine(t *testing.T) {
	o, _, _ := testOnDemand(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" && r.URL.Path == "/v1/capture" {
			w.Write([]byte(`{"models":[{"model":"nvidia/Qwen3.8-Flash-Next-NVFP4","family":"qwen4_exp","capture":true,"num_layers":48,"hidden_size":4096},{"model":"x/no","capture":false}]}`))
			return
		}
		w.WriteHeader(404)
		w.Write([]byte(`{"error":{"type":"model_not_found","message":"capture model vendor/test-model is not served here"}}`))
	})
	_, err := o.capture(context.Background(), "text", testTarget)
	message, retry := scoringFailure(err)
	if message != "Dictionary 0123456789ab belongs to vendor/test-model layer 19; the capture engine does not serve that model (it serves nvidia/Qwen3.8-Flash-Next-NVFP4)." || retry {
		t.Fatalf("404: %q retry=%v", message, retry)
	}
	o, _, _ = testOnDemand(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(501)
		w.Write([]byte(`{"error":{"type":"capture_error","message":"layer capture is not available on this engine (gpt_oss)"}}`))
	})
	_, err = o.capture(context.Background(), "text", testTarget)
	message, retry = scoringFailure(err)
	if message != "Dictionary 0123456789ab belongs to vendor/test-model layer 19; the capture engine serving it cannot capture layers (HTTP 501)." || retry {
		t.Fatalf("501: %q retry=%v", message, retry)
	}
}

func TestCaptureFallsBackToLagunaRouteOnOlderEngine(t *testing.T) {
	var paths []string
	o, _, _ := testOnDemand(t, func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		if r.URL.Path == "/v1/capture" {
			w.WriteHeader(404)
			return
		}
		captureOK(w, r)
	})
	mean, err := o.capture(context.Background(), "text", testTarget)
	if err != nil || len(mean) != 2 || strings.Join(paths, " ") != "/v1/capture /v1/laguna/capture" {
		t.Fatalf("fallback: %v %v %v", mean, err, paths)
	}
}

func TestCaptureNamesUnservedModelThroughLagunaAlias(t *testing.T) {
	o, _, _ := testOnDemand(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/laguna/capture" {
			w.WriteHeader(404)
			w.Write([]byte(`{"error":{"type":"model_not_found","message":"not here"}}`))
			return
		}
		w.WriteHeader(404)
		w.Write([]byte(`<html>404 Not Found</html>`))
	})
	_, err := o.capture(context.Background(), "text", testTarget)
	message, retry := scoringFailure(err)
	if message != "Dictionary 0123456789ab belongs to vendor/test-model layer 19; the capture engine does not serve that model." || retry {
		t.Fatalf("alias 404: %q retry=%v", message, retry)
	}
}
