package state

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
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
