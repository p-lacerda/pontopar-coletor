package state

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/p-lacerda/pontopar-coletor/internal/audit"
)

func TestSettersAndSnapshot(t *testing.T) {
	dir := t.TempDir()
	s := New(dir, "v1.2.3")
	s.SetEndpoints("http://192.168.1.111:90", "https://thera")
	s.DeviceOK()
	s.TheraOK()
	s.SetCursor(100)
	s.SeenDeviceID(150)
	s.Forwarded(3)
	s.SkippedNoTime(1)
	s.RecordError("boom")

	snap := s.Get()
	if snap.Version != "v1.2.3" {
		t.Errorf("version: %q", snap.Version)
	}
	if !snap.DeviceOnline || !snap.TheraOnline {
		t.Errorf("online flags: device=%v thera=%v", snap.DeviceOnline, snap.TheraOnline)
	}
	if snap.Cursor != 100 || snap.MaxDeviceID != 150 {
		t.Errorf("cursor/max: %d/%d", snap.Cursor, snap.MaxDeviceID)
	}
	if snap.ForwardedTotal != 3 || snap.SkippedNoTime != 1 {
		t.Errorf("counters: fwd=%d skip=%d", snap.ForwardedTotal, snap.SkippedNoTime)
	}
	if snap.ErrorCount != 1 || snap.LastError != "boom" {
		t.Errorf("error: count=%d last=%q", snap.ErrorCount, snap.LastError)
	}
}

func TestPersistToFileOnFlush(t *testing.T) {
	dir := t.TempDir()
	s := New(dir, "vX")
	s.SetCursor(42)
	s.Flush()

	data, err := os.ReadFile(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatalf("state.json não gravado: %v", err)
	}
	var snap Snapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		t.Fatalf("state.json inválido: %v", err)
	}
	if snap.Cursor != 42 {
		t.Errorf("cursor persistido: quero 42, veio %d", snap.Cursor)
	}
}

func TestFullSyncLifecycle(t *testing.T) {
	s := New(t.TempDir(), "v")
	s.FullSyncStart()
	if !s.Get().FullSyncRunning {
		t.Fatal("full-sync deveria estar rodando")
	}
	s.FullSyncProgress(500, 480, 500)
	snap := s.Get()
	if snap.FullSyncCurrentID != 500 || snap.FullSyncForwarded != 480 {
		t.Fatalf("progresso inesperado: %+v", snap)
	}
	s.FullSyncDone("ok: 480 enviadas")
	snap = s.Get()
	if snap.FullSyncRunning || snap.FullSyncLastResult != "ok: 480 enviadas" {
		t.Fatalf("done inesperado: %+v", snap)
	}
}

func TestRecordPunchTodayRingAndRestore(t *testing.T) {
	dir := t.TempDir()
	s := New(dir, "v")
	now := time.Now()
	for i := 0; i < recentMax+5; i++ {
		s.RecordPunch(Punch{ID: int64(i + 1), UserID: "u", Registration: "0003", Time: now.Unix()})
	}
	s.RecordPunch(Punch{ID: 999, UserID: "x", Time: now.AddDate(0, 0, -2).Unix()}) // antiga e sem matrícula
	snap := s.Get()
	if len(snap.RecentPunches) != recentMax || snap.RecentPunches[0].ID != 999 {
		t.Fatalf("feed deveria ter %d itens com o mais novo primeiro; veio %d (topo %d)", recentMax, len(snap.RecentPunches), snap.RecentPunches[0].ID)
	}
	if snap.ForwardedToday != int64(recentMax+5) {
		t.Errorf("batidas hoje: quero %d (a de 2 dias atrás não conta), veio %d", recentMax+5, snap.ForwardedToday)
	}
	if snap.NoRegistration != 1 {
		t.Errorf("sem matrícula: quero 1, veio %d", snap.NoRegistration)
	}
	s.FullSyncDone("ok", audit.Record{Forwarded: 10})
	s.Flush()

	// "Reinício do Windows": um estado novo na mesma pasta restaura o dia.
	s2 := New(dir, "v")
	snap2 := s2.Get()
	if snap2.ForwardedToday != snap.ForwardedToday || len(snap2.RecentPunches) != recentMax {
		t.Errorf("restauração: hoje=%d feed=%d", snap2.ForwardedToday, len(snap2.RecentPunches))
	}
	if snap2.FullSyncLast == nil || snap2.FullSyncLast.Forwarded != 10 {
		t.Errorf("último refill não restaurado: %+v", snap2.FullSyncLast)
	}

	// Virou o dia: "batidas hoje" zera sozinho na leitura.
	s2.now = func() time.Time { return now.AddDate(0, 0, 1) }
	if got := s2.Get().ForwardedToday; got != 0 {
		t.Errorf("no dia seguinte, batidas hoje deveria ser 0, veio %d", got)
	}
}
