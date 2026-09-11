package idface

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

type nopLogger struct{}

func (nopLogger) Infof(string, ...any)  {}
func (nopLogger) Errorf(string, ...any) {}

func TestFlexStrAcceptsNumberAndString(t *testing.T) {
	var l AccessLog
	raw := `{"id":123,"time":"1700000000","event":"7","device_id":5,"user_id":"9","confidence":100}`
	if err := json.Unmarshal([]byte(raw), &l); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if l.Id != "123" {
		t.Errorf("id numerico: quero \"123\", veio %q", l.Id)
	}
	if l.Time != "1700000000" {
		t.Errorf("time string: quero \"1700000000\", veio %q", l.Time)
	}
	if l.DeviceId != "5" {
		t.Errorf("device_id numerico: quero \"5\", veio %q", l.DeviceId)
	}
	if l.UserId != "9" {
		t.Errorf("user_id string: quero \"9\", veio %q", l.UserId)
	}
	if l.Confidence != "100" {
		t.Errorf("confidence numerico: quero \"100\", veio %q", l.Confidence)
	}

	id, err := l.IdNum()
	if err != nil || id != 123 {
		t.Errorf("IdNum: quero 123,nil; veio %d,%v", id, err)
	}
}

func TestIdNumRejectsInvalid(t *testing.T) {
	l := AccessLog{Id: "abc"}
	if _, err := l.IdNum(); err == nil {
		t.Error("IdNum deveria falhar para id nao numerico")
	}
	l2 := AccessLog{Id: ""}
	if _, err := l2.IdNum(); err == nil {
		t.Error("IdNum deveria falhar para id vazio")
	}
}

func TestResolveRegistrationsCachesAndMarksMissing(t *testing.T) {
	// Servidor fake que responde ao load_objects de users.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req map[string]any
		_ = json.Unmarshal(body, &req)
		// Só devolve o user 10 (matricula "0001"); o user 20 fica sem matricula.
		_, _ = w.Write([]byte(`{"users":[{"id":10,"registration":"0001"}]}`))
	}))
	defer srv.Close()

	c := New(srv.URL, "admin", "admin", &http.Client{Timeout: 2 * time.Second}, nopLogger{})
	// Simula sessão válida sem chamar Login.
	c.session = "sess"

	c.ResolveRegistrations(context.Background(), []string{"10", "20", "10"})

	if got := c.Registration("10"); got != "0001" {
		t.Errorf("user 10: quero \"0001\", veio %q", got)
	}
	// user 20 nao voltou: marcado como "" (consultado, sem matricula).
	if got := c.Registration("20"); got != "" {
		t.Errorf("user 20: quero \"\", veio %q", got)
	}

	// Segunda chamada nao deve reconsultar (ambos ja cacheados). Fecha o server
	// para garantir que nenhuma request nova acontece.
	srv.Close()
	c.ResolveRegistrations(context.Background(), []string{"10", "20"})
	if got := c.Registration("10"); got != "0001" {
		t.Errorf("apos 2a chamada, user 10 mudou: %q", got)
	}
}

func TestSyncClockPostsExpectedFields(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/session_is_valid.fcgi" {
			_, _ = w.Write([]byte(`{"session_is_valid":true}`))
			return
		}
		if r.URL.Path != "/set_system_time.fcgi" || r.URL.Query().Get("session") != "sess 1" {
			t.Fatalf("endpoint/sessão inesperados: %s", r.URL.String())
		}
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &got); err != nil {
			t.Fatalf("body inválido: %v", err)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	c := New(srv.URL, "admin", "admin", &http.Client{Timeout: 2 * time.Second}, nopLogger{})
	c.session = "sess 1"
	now := time.Date(2026, 9, 11, 8, 3, 7, 0, time.FixedZone("BRT", -3*60*60))
	if err := c.SyncClock(context.Background(), now); err != nil {
		t.Fatalf("SyncClock: %v", err)
	}
	for k, want := range map[string]float64{"day": 11, "month": 9, "year": 2026, "hour": 8, "minute": 3, "second": 7} {
		if got[k] != want {
			t.Errorf("%s: quero %v, veio %v", k, want, got[k])
		}
	}
}

func TestSetFacialConfigurationDefaultsToAttendance(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/session_is_valid.fcgi" {
			_, _ = w.Write([]byte(`{"session_is_valid":true}`))
			return
		}
		data, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(data, &body); err != nil {
			t.Fatalf("body inválido: %v", err)
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	c := New(srv.URL, "admin", "admin", &http.Client{Timeout: 2 * time.Second}, nopLogger{})
	c.session = "sess"
	if err := c.SetFacialConfiguration(context.Background(), true, true, true, 50, ""); err != nil {
		t.Fatalf("attendance padrão: %v", err)
	}
	general, ok := body["general"].(map[string]any)
	if !ok || general["attendance_mode"] != "1" {
		t.Fatalf("attendance_mode padrão deveria ser 1, body=%v", body)
	}
	if err := c.SetFacialConfiguration(context.Background(), true, true, true, 50, "access"); err != nil {
		t.Fatalf("access explícito: %v", err)
	}
	general, ok = body["general"].(map[string]any)
	if !ok || general["attendance_mode"] != "0" {
		t.Fatalf("access deveria ser 0, body=%v", body)
	}
}
