package audit

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAppendAndRecent(t *testing.T) {
	dir := t.TempDir()
	l := New(dir)
	for i := 0; i < 5; i++ {
		if err := l.Append(Record{Mode: ModeIncremental, FromID: int64(i), ToID: int64(i + 1), Read: 1, Forwarded: 1}); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}
	recent, err := l.Recent(3)
	if err != nil {
		t.Fatalf("recent: %v", err)
	}
	if len(recent) != 3 {
		t.Fatalf("quero 3 recentes, veio %d", len(recent))
	}
	// Ordem cronológica crescente: os 3 últimos têm FromID 2,3,4.
	if recent[0].FromID != 2 || recent[2].FromID != 4 {
		t.Fatalf("ordem inesperada: %+v", recent)
	}
}

func TestReadAllIgnoresPartialLine(t *testing.T) {
	dir := t.TempDir()
	l := New(dir)
	if err := l.Append(Record{Mode: ModeFull, ToID: 10, Forwarded: 10, Verified: true}); err != nil {
		t.Fatal(err)
	}
	// Simula uma última linha parcial (processo morto no meio do append).
	f, err := os.OpenFile(l.Path(), os.O_WRONLY|os.O_APPEND, 0o640)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString(`{"mode":"full","toId":`)
	_ = f.Close()

	all, err := ReadAll(l.Path())
	if err != nil {
		t.Fatalf("readall: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("linha parcial deveria ser ignorada; quero 1 record, veio %d", len(all))
	}
	if !all[0].Verified || all[0].ToID != 10 {
		t.Fatalf("record válido corrompido: %+v", all[0])
	}
}

func TestReadAllMissingFile(t *testing.T) {
	all, err := ReadAll(filepath.Join(t.TempDir(), "nao-existe.jsonl"))
	if err != nil {
		t.Fatalf("arquivo ausente não deveria dar erro: %v", err)
	}
	if len(all) != 0 {
		t.Fatalf("quero vazio, veio %d", len(all))
	}
}

func TestExportCSV(t *testing.T) {
	recs := []Record{
		{Time: time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC), Mode: ModeFull, FromID: 0, ToID: 1234, Read: 1200, Forwarded: 1198, SkippedNoTime: 2, Verified: true, DurationMs: 5000},
	}
	var buf bytes.Buffer
	if err := ExportCSV(&buf, recs); err != nil {
		t.Fatalf("exportcsv: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "time,mode,fromId,toId") {
		t.Fatalf("cabeçalho ausente: %q", out)
	}
	if !strings.Contains(out, "2026-11-03T12:00:00Z,full,0,1234") {
		t.Fatalf("linha de dados inesperada: %q", out)
	}
	if !strings.Contains(out, ",true,5000,,") {
		t.Fatalf("verified/duração inesperados: %q", out)
	}
}
