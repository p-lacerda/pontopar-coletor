package store

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCursorRoundTrip(t *testing.T) {
	dir := t.TempDir()
	c := NewCursor(dir)

	if got := c.Get(); got != 0 {
		t.Fatalf("cursor inicial: quero 0, veio %d", got)
	}
	if err := c.Set(42); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if got := c.Get(); got != 42 {
		t.Fatalf("apos Set(42): quero 42, veio %d", got)
	}

	// Novo cursor no mesmo dir deve ler o valor persistido.
	c2 := NewCursor(dir)
	if got := c2.Get(); got != 42 {
		t.Fatalf("persistencia: quero 42, veio %d", got)
	}
}

func TestCursorCorruptFallsBackToZero(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "cursor.json"), []byte("{lixo"), 0o644); err != nil {
		t.Fatal(err)
	}
	c := NewCursor(dir)
	if got := c.Get(); got != 0 {
		t.Fatalf("cursor corrompido deve cair para 0, veio %d", got)
	}
}

func TestCursorNoLeftoverTempFiles(t *testing.T) {
	dir := t.TempDir()
	c := NewCursor(dir)
	for i := int64(1); i <= 5; i++ {
		if err := c.Set(i); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != "cursor.json" {
			t.Fatalf("arquivo inesperado no dir apos Sets: %s", e.Name())
		}
	}
}
