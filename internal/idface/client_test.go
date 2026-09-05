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
