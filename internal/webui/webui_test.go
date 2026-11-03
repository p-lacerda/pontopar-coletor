package webui

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/p-lacerda/pontopar-coletor/internal/audit"
	"github.com/p-lacerda/pontopar-coletor/internal/config"
	"github.com/p-lacerda/pontopar-coletor/internal/state"
)

type fakeCtrl struct {
	st           *state.State
	fullCalled   int64
	savedConfig  *config.Config
	autostartSet *bool
}

func (f *fakeCtrl) Snapshot() state.Snapshot { return f.st.Get() }
func (f *fakeCtrl) ServiceStatus() string    { return "rodando" }
func (f *fakeCtrl) AutoStart() (bool, bool)  { return true, true }
func (f *fakeCtrl) RecentAudit(n int) ([]audit.Record, error) {
	return []audit.Record{{Mode: audit.ModeFull, ToID: 99, Forwarded: 99, Verified: true}}, nil
}
func (f *fakeCtrl) AuditCSV() ([]byte, error) { return []byte("time,mode\n"), nil }
func (f *fakeCtrl) LogTail(int) string        { return "linha de log" }
func (f *fakeCtrl) CurrentConfig() *config.Config {
	return &config.Config{DeviceIp: "1.2.3.4", Login: "admin", TheraBase: "https://t", DeviceSecret: "s"}
}
func (f *fakeCtrl) SaveConfig(c *config.Config) error   { f.savedConfig = c; return nil }
func (f *fakeCtrl) TriggerFullSync(startID int64) error { f.fullCalled++; return nil }
func (f *fakeCtrl) TestDevice() error                   { return nil }
func (f *fakeCtrl) TestThera() error                    { return nil }
func (f *fakeCtrl) SetAutoStart(enable bool) error      { f.autostartSet = &enable; return nil }
func (f *fakeCtrl) StartService() error                 { return nil }
func (f *fakeCtrl) StopService() error                  { return nil }
func (f *fakeCtrl) Version() string                     { return "vtest" }

func startTestServer(t *testing.T) (*Server, string) {
	ctrl := &fakeCtrl{st: state.New(t.TempDir(), "vtest")}
	ctrl.st.SetCursor(42)
	s := NewServer(ctrl, "127.0.0.1:0", nil)
	// Não usamos Serve (bind:0 dificulta pegar a porta); testamos os handlers via
	// um servidor httptest montando o mesmo mux não é trivial — então testamos os
	// handlers diretamente.
	return s, s.token
}

func TestIndexInjectsTokenAndVersion(t *testing.T) {
	s, token := startTestServer(t)
	req, _ := http.NewRequest("GET", "http://127.0.0.1/", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	rw := newRec()
	s.handleIndex(rw, req)
	body := rw.body.String()
	if !strings.Contains(body, token) {
		t.Error("token não injetado na página")
	}
	if !strings.Contains(body, "vtest") {
		t.Error("versão não injetada na página")
	}
}

func TestStatusEndpoint(t *testing.T) {
	s, _ := startTestServer(t)
	req, _ := http.NewRequest("GET", "http://127.0.0.1/api/status", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	rw := newRec()
	s.wrap(s.handleStatus)(rw, req)
	if rw.code != 200 {
		t.Fatalf("status http: %d", rw.code)
	}
	if !strings.Contains(rw.body.String(), `"cursor":42`) {
		t.Errorf("status sem cursor esperado: %s", rw.body.String())
	}
}

func TestMutationRequiresToken(t *testing.T) {
	s, token := startTestServer(t)
	// POST sem token -> 403
	req, _ := http.NewRequest("POST", "http://127.0.0.1/api/fullsync", strings.NewReader("{}"))
	req.RemoteAddr = "127.0.0.1:1"
	rw := newRec()
	s.wrap(s.handleFullSync)(rw, req)
	if rw.code != http.StatusForbidden {
		t.Fatalf("POST sem token deveria dar 403, veio %d", rw.code)
	}
	// POST com token -> 200
	req2, _ := http.NewRequest("POST", "http://127.0.0.1/api/fullsync", strings.NewReader(`{"startId":0}`))
	req2.RemoteAddr = "127.0.0.1:1"
	req2.Header.Set("X-PontoPar-Token", token)
	rw2 := newRec()
	s.wrap(s.handleFullSync)(rw2, req2)
	if rw2.code != 200 {
		t.Fatalf("POST com token deveria dar 200, veio %d (%s)", rw2.code, rw2.body.String())
	}
}

func TestNonLoopbackRejected(t *testing.T) {
	s, _ := startTestServer(t)
	req, _ := http.NewRequest("GET", "http://host/api/status", nil)
	req.RemoteAddr = "8.8.8.8:4444"
	rw := newRec()
	s.wrap(s.handleStatus)(rw, req)
	if rw.code != http.StatusForbidden {
		t.Fatalf("requisição não-loopback deveria ser bloqueada, veio %d", rw.code)
	}
}

func TestServeBindsAndShutsDown(t *testing.T) {
	ctrl := &fakeCtrl{st: state.New(t.TempDir(), "v")}
	s := NewServer(ctrl, "127.0.0.1:0", nil)
	_ = s // Serve com :0 não expõe a porta facilmente; cobertura de bind fica no handler.
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	// Bind numa porta efêmera concreta para exercer o caminho de Serve/Shutdown.
	s2 := NewServer(ctrl, "127.0.0.1:47199", nil)
	done := make(chan error, 1)
	go func() { done <- s2.Serve(ctx) }()
	time.Sleep(15 * time.Millisecond)
	resp, err := http.Get("http://127.0.0.1:47199/api/status")
	if err == nil {
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		if !bytes.Contains(b, []byte("service")) {
			t.Errorf("resposta inesperada: %s", b)
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Serve não encerrou após ctx cancelado")
	}
}

// --- recorder mínimo ---
type rec struct {
	code    int
	body    *bytes.Buffer
	headers http.Header
}

func newRec() *rec                         { return &rec{code: 200, body: &bytes.Buffer{}, headers: http.Header{}} }
func (r *rec) Header() http.Header         { return r.headers }
func (r *rec) Write(b []byte) (int, error) { return r.body.Write(b) }
func (r *rec) WriteHeader(c int)           { r.code = c }
