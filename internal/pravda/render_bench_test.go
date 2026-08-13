package pravda

import (
	"io"
	"os"
	"path/filepath"
	"testing"
)

func loadFixture(b *testing.B, name string) *Session {
	b.Helper()
	f, err := os.Open(filepath.Join("fixtures", name))
	if err != nil {
		if os.IsNotExist(err) {
			b.Skipf("fixture %s not present: %v", name, err)
		}
		b.Fatal(err)
	}
	defer f.Close()
	events, err := ParseJSONL(f)
	if err != nil {
		b.Fatal(err)
	}
	sess := Normalize(events, nil)
	b.Logf("entries: %d, events: %d", len(sess.Entries), len(events))
	return sess
}

func loadFixtureSession(b *testing.B) *Session {
	return loadFixture(b, "sample1.jsonl")
}

func BenchmarkRenderEntries(b *testing.B) {
	sess := loadFixtureSession(b)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for j := range sess.Entries {
			renderSingleEntry(&sess.Entries[j])
		}
	}
}

func BenchmarkRenderBaseLarge(b *testing.B) {
	sess := loadFixture(b, "large-session.jsonl")
	pd := pageData{
		Title:   "bench",
		Session: sess,
		SessionFile: &fileInfo{
			Name: "bench.jsonl",
			Path: "bench.jsonl",
		},
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		renderBase(io.Discard, pd)
	}
}

func BenchmarkRenderBase(b *testing.B) {
	sess := loadFixtureSession(b)
	pd := pageData{
		Title:   "bench",
		Session: sess,
		SessionFile: &fileInfo{
			Name: "bench.jsonl",
			Path: "bench.jsonl",
		},
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		renderBase(io.Discard, pd)
	}
}

func BenchmarkNormalize(b *testing.B) {
	f, err := os.Open(filepath.Join("fixtures", "sample1.jsonl"))
	if err != nil {
		b.Fatal(err)
	}
	defer f.Close()
	events, _ := ParseJSONL(f)
	b.Logf("events: %d", len(events))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		Normalize(events, nil)
	}
}

func BenchmarkMarkdown(b *testing.B) {
	texts := []string{
		"Simple paragraph with **bold** and `code`.",
		"# Heading\n\nParagraph one.\n\nParagraph two with [link](http://example.com).\n\n```go\nfunc main() {}\n```",
		"Long text. " + string(make([]byte, 4000)),
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for _, t := range texts {
			renderMarkdown(t)
		}
	}
}

func BenchmarkRawField(b *testing.B) {

	sess := loadFixtureSession(b)
	var toolEntries []Entry
	for _, e := range sess.Entries {
		if e.Kind == KindTool && len(e.Input) > 0 {
			toolEntries = append(toolEntries, e)
		}
	}
	b.Logf("tool entries: %d", len(toolEntries))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for j := range toolEntries {
			rawField(toolEntries[j].Input, "command")
			rawField(toolEntries[j].Input, "description")
			rawField(toolEntries[j].Input, "file_path")
		}
	}
}
