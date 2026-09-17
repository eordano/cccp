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
				json.NewEncoder(w).Encode(map[string]any{"layer": 19, "hidden_size": 2, "records": []map[string]any{{"tokens": tokens, "mean": []float64{value, 2 * value}}}})
			})
			mean, err := o.capture(context.Background(), text, 19)
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
	_, err := o.capture(context.Background(), "a valid message", 19)
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
	mean, err := o.capture(context.Background(), "a valid long message", 19)
	if err == nil || len(mean) != 0 || calls != 3 {
		t.Fatalf("partial capture returned as success: %v %v %d", mean, err, calls)
	}
}
