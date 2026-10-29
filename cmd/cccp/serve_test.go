package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadReadToken(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	if tok, err := loadReadToken("", "full", "push"); err != nil || tok != "" {
		t.Errorf("unset: got %q, %v", tok, err)
	}
	if tok, err := loadReadToken(write("ok", "reader\n"), "full", "push"); err != nil || tok != "reader" {
		t.Errorf("valid: got %q, %v", tok, err)
	}
	for name, body := range map[string]string{"empty": " \n", "same-full": "full", "same-push": "push"} {
		if _, err := loadReadToken(write(name, body), "full", "push"); err == nil {
			t.Errorf("%s: want error", name)
		}
	}
	if _, err := loadReadToken(filepath.Join(dir, "missing"), "full", "push"); err == nil {
		t.Error("missing file: want error")
	}
}
