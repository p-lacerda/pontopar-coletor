package webui

import (
	"bytes"
	"context"
	"encoding/json"
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
	lastFull     [3]int64
	lastSource   string
	collected    int
	savedConfig  *config.Config
	autostartSet *bool
}

func (f *fakeCtrl) Mode() string             { return ModeService }
func (f *fakeCtrl) Snapshot() state.Snapshot { return f.st.Get() }
func (f *fakeCtrl) ServiceStatus() string    { return "rodando" }
func (f *fakeCtrl) AutoStart() (bool, bool)  { return true, true }
func (f *fakeCtrl) TrayAutoStart() bool      { return true }
func (f *fakeCtrl) DataDir() string          { return `C:\ProgramData\PontoParColetor` }
func (f *fakeCtrl) RecentAudit(n int) ([]audit.Record, error) {
	all := []audit.Record{
		{Mode: audit.ModeFull, ToID: 10},
		{Mode: audit.ModeIncremental, ToID: 11},
		{Mode: audit.ModeFullPartial, ToID: 12},
		{Mode: audit.ModeIncremental, ToID: 13},
		{Mode: audit.ModeFull, ToID: 99, Forwarded: 99, Verified: true},
	}
	if n > 0 && len(all) > n {
		all = all[len(all)-n:]
	}
	return all, nil
}
func (f *fakeCtrl) AuditCSV() ([]byte, error) { return []byte("time,mode\n"), nil }
func (f *fakeCtrl) LogTail(int) string        { return "linha de log" }
func (f *fakeCtrl) CurrentConfig() *config.Config {
	return &config.Config{DeviceIp: "1.2.3.4", Login: "admin", TheraBase: "https://t", DeviceSecret: "s"}
}
func (f *fakeCtrl) SaveConfig(c *config.Config) error { f.savedConfig = c; return nil }
func (f *fakeCtrl) TriggerFullSync(startID, fromUnix, toUnix int64, source string) error {
	f.fullCalled++
	f.lastFull = [3]int64{startID, fromUnix, toUnix}
	f.lastSource = source
	return nil
}
func (f *fakeCtrl) CollectNow() error               { f.collected++; return nil }
func (f *fakeCtrl) CancelRefill() error             { return nil }
func (f *fakeCtrl) Restart() error                  { return nil }
func (f *fakeCtrl) TestDevice(*config.Config) error { return nil }
func (f *fakeCtrl) TestThera(*config.Config) error  { return nil }
func (f *fakeCtrl) SetAutoStart(enable bool) error  { f.autostartSet = &enable; return nil }
func (f *fakeCtrl) StartService() error             { return nil }
func (f *fakeCtrl) StopService() error              { return nil }
func (f *fakeCtrl) Install() error                  { return nil }
func (f *fakeCtrl) Version() string                 { return "vtest" }

func startTestServer(t *testing.T) (*Server, string) {
	s, _ := startTestServerCtrl(t)
	return s, s.token
}

func startTestServerCtrl(t *testing.T) (*Server, *fakeCtrl) {
	ctrl := &fakeCtrl{st: state.New(t.TempDir(), "vtest")}
	ctrl.st.SetCursor(42)
	s := NewServer(ctrl, "127.0.0.1:0", nil)
	return s, ctrl
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
	if !strings.Contains(body, `content="service"`) || strings.Contains(body, "{{") {
		t.Error("modo não injetado na página (ou sobrou marcador)")
	}
}

// Um site malicioso que aponta o próprio domínio para 127.0.0.1 (DNS rebinding)
// chega com Host de outro domínio: tem de ser recusado mesmo vindo de loopback.
func TestDNSRebindingHostRejected(t *testing.T) {
	s, _ := startTestServer(t)
	for _, path := range []string{"/api/config", "/"} {
		req, _ := http.NewRequest("GET", "http://evil.example:47114"+path, nil)
		req.RemoteAddr = "127.0.0.1:5555"
		rw := newRec()
		s.Handler().ServeHTTP(rw, req)
		if rw.code != http.StatusForbidden {
			t.Errorf("%s com Host estranho deveria dar 403, veio %d", path, rw.code)
		}
	}
	req, _ := http.NewRequest("GET", "http://localhost:47114/api/status", nil)
	req.RemoteAddr = "127.0.0.1:5555"
	rw := newRec()
	s.Handler().ServeHTTP(rw, req)
	if rw.code != 200 {
		t.Errorf("Host localhost deveria passar, veio %d", rw.code)
	}
}

func TestRefillEndpointModes(t *testing.T) {
	s, ctrl := startTestServerCtrl(t)
	now := time.Date(2026, 11, 11, 15, 0, 0, 0, time.Local)
	s.now = func() time.Time { return now }
	cases := []struct {
		body     string
		want     [3]int64
		wantCode int
	}{
		{`{"mode":"all"}`, [3]int64{0, 0, 0}, 200},
		{`{"mode":"resume"}`, [3]int64{-1, 0, 0}, 200},
		{`{"mode":"since","from":"2026-11-03"}`, [3]int64{0, time.Date(2026, 11, 3, 0, 0, 0, 0, time.Local).Unix(), 0}, 200},
		{`{"mode":"period","from":"2026-11-03","to":"2026-11-03"}`, [3]int64{0, time.Date(2026, 11, 3, 0, 0, 0, 0, time.Local).Unix(), time.Date(2026, 11, 3, 23, 59, 59, 0, time.Local).Unix()}, 200},
		{`{"mode":"days","days":7}`, [3]int64{0, time.Date(2026, 11, 5, 0, 0, 0, 0, time.Local).Unix(), 0}, 200},
		{`{"mode":"since"}`, [3]int64{}, 400},
		{`{"mode":"period","from":"2026-11-05","to":"2026-11-01"}`, [3]int64{}, 400},
		{`{"mode":"xpto"}`, [3]int64{}, 400},
	}
	for _, c := range cases {
		before := ctrl.fullCalled
		req, _ := http.NewRequest("POST", "http://127.0.0.1/api/refill", strings.NewReader(c.body))
		req.RemoteAddr = "127.0.0.1:1"
		req.Header.Set(TokenHeader, s.token)
		rw := newRec()
		s.Handler().ServeHTTP(rw, req)
		if rw.code != c.wantCode {
			t.Errorf("%s: http %d, esperado %d (%s)", c.body, rw.code, c.wantCode, rw.body.String())
			continue
		}
		if c.wantCode != 200 {
			if ctrl.fullCalled != before {
				t.Errorf("%s: não deveria disparar refill", c.body)
			}
			continue
		}
		if ctrl.lastFull != c.want {
			t.Errorf("%s: refill(%v), esperado %v", c.body, ctrl.lastFull, c.want)
		}
		if ctrl.lastSource != "painel" {
			t.Errorf("%s: origem %q, esperado painel", c.body, ctrl.lastSource)
		}
	}
}

func TestCollectNowRequiresPost(t *testing.T) {
	s, ctrl := startTestServerCtrl(t)
	req, _ := http.NewRequest("GET", "http://127.0.0.1/api/collect-now", nil)
	req.RemoteAddr = "127.0.0.1:1"
	rw := newRec()
	s.Handler().ServeHTTP(rw, req)
	if rw.code != http.StatusMethodNotAllowed || ctrl.collected != 0 {
		t.Fatalf("GET em collect-now deveria dar 405 sem coletar, veio %d (coletou %d)", rw.code, ctrl.collected)
	}
	req, _ = http.NewRequest("POST", "http://127.0.0.1/api/collect-now", nil)
	req.RemoteAddr = "127.0.0.1:1"
	req.Header.Set(TokenHeader, s.token)
	rw = newRec()
	s.Handler().ServeHTTP(rw, req)
	if rw.code != 200 || ctrl.collected != 1 {
		t.Fatalf("POST em collect-now deveria coletar, veio %d (coletou %d)", rw.code, ctrl.collected)
	}
}

func TestLogoServed(t *testing.T) {
	s, _ := startTestServer(t)
	req, _ := http.NewRequest("GET", "http://127.0.0.1/logo.png", nil)
	req.RemoteAddr = "127.0.0.1:1"
	rw := newRec()
	s.Handler().ServeHTTP(rw, req)
	if rw.code != 200 || !bytes.HasPrefix(rw.body.Bytes(), []byte("\x89PNG")) {
		t.Fatalf("logo não servido como PNG: %d", rw.code)
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

// O histórico de refills filtra no servidor: incrementais não empurram os
// refills para fora da lista.
func TestAuditModeFilter(t *testing.T) {
	s, _ := startTestServer(t)
	req, _ := http.NewRequest("GET", "http://127.0.0.1/api/audit?mode=full&n=2", nil)
	req.RemoteAddr = "127.0.0.1:1"
	rw := newRec()
	s.Handler().ServeHTTP(rw, req)
	var recs []audit.Record
	if err := json.Unmarshal(rw.body.Bytes(), &recs); err != nil {
		t.Fatal(err)
	}
	if len(recs) != 2 || recs[0].ToID != 99 || recs[1].ToID != 12 {
		t.Fatalf("esperava os 2 refills mais recentes (99, 12), veio %+v", recs)
	}
}
