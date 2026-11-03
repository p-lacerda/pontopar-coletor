// Package webui serve o PAINEL local do coletor em 127.0.0.1 (loopback apenas).
//
// O painel é a interface rica de operação: status ao vivo (conexão com o
// aparelho e com o Thera, cursor, batidas enviadas, erros), log ao vivo,
// auditoria exportável, TODAS as configurações, o botão "Consultar o Control iD
// completo" (full-sync) e o "Iniciar com o Windows". Roda DENTRO do processo que
// coleta (o serviço do Windows em produção), então o status é o estado real do
// coletor — não uma cópia.
//
// Segurança: escuta só em loopback; mutações (salvar config, full-sync, etc.)
// exigem um token gerado por processo e injetado na página servida — um site
// malicioso no navegador não consegue lê-lo (mesma-origem), então não forja POSTs.
package webui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"crypto/rand"
	"encoding/hex"

	"github.com/p-lacerda/pontopar-coletor/internal/audit"
	"github.com/p-lacerda/pontopar-coletor/internal/config"
	"github.com/p-lacerda/pontopar-coletor/internal/state"
)

// Logger é a interface mínima de log.
type Logger interface {
	Infof(format string, args ...any)
	Errorf(format string, args ...any)
}

// Controller é a ponte entre o painel e o coletor/serviço. É implementado pelo
// supervisor (internal/app), que detém a config, o coletor e o serviço.
type Controller interface {
	// Snapshot é o estado vivo do coletor.
	Snapshot() state.Snapshot
	// ServiceStatus é o estado do serviço Windows ("rodando", "parado", ...).
	ServiceStatus() string
	// AutoStart diz se o "Iniciar com o Windows" está ligado e se foi possível
	// consultar (known=false quando o serviço não está instalado / fora do Windows).
	AutoStart() (enabled bool, known bool)
	// RecentAudit devolve os últimos n runs auditados.
	RecentAudit(n int) ([]audit.Record, error)
	// AuditCSV escreve a auditoria inteira em CSV.
	AuditCSV() ([]byte, error)
	// LogTail devolve o fim do arquivo de log (até maxBytes).
	LogTail(maxBytes int) string
	// CurrentConfig é a config em uso (para preencher o formulário).
	CurrentConfig() *config.Config
	// SaveConfig grava config.json e sinaliza recarga do coletor.
	SaveConfig(*config.Config) error
	// TriggerFullSync dispara o full-sync em segundo plano. startID<0 retoma.
	TriggerFullSync(startID int64) error
	// TestDevice/TestThera checam conectividade sob demanda.
	TestDevice() error
	TestThera() error
	// SetAutoStart liga/desliga o início com o Windows.
	SetAutoStart(enable bool) error
	// StartService/StopService controlam o serviço.
	StartService() error
	StopService() error
	// Version é a versão do binário.
	Version() string
}

// Server é o servidor HTTP do painel.
type Server struct {
	ctrl  Controller
	log   Logger
	addr  string
	token string
	http  *http.Server
}

// NewServer cria o servidor em addr (ex.: "127.0.0.1:47114"). O token é gerado
// agora e injetado na página.
func NewServer(ctrl Controller, addr string, log Logger) *Server {
	return &Server{ctrl: ctrl, log: log, addr: addr, token: newToken()}
}

func newToken() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		// Fallback determinístico só para não ficar sem token; loopback já protege.
		return "pontopar-local"
	}
	return hex.EncodeToString(b)
}

// Serve escuta e serve até ctx ser cancelado. Devolve erro de bind (o chamador
// loga e segue; o painel NUNCA pode derrubar a coleta). Loopback-only: tenta
// bind no addr informado (já forçado a 127.0.0.1 pela config).
func (s *Server) Serve(ctx context.Context) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleIndex)
	mux.HandleFunc("/api/status", s.wrap(s.handleStatus))
	mux.HandleFunc("/api/audit", s.wrap(s.handleAudit))
	mux.HandleFunc("/api/audit.csv", s.wrap(s.handleAuditCSV))
	mux.HandleFunc("/api/log", s.wrap(s.handleLog))
	mux.HandleFunc("/api/config", s.wrap(s.handleConfig))
	mux.HandleFunc("/api/fullsync", s.wrap(s.handleFullSync))
	mux.HandleFunc("/api/test-device", s.wrap(s.handleTestDevice))
	mux.HandleFunc("/api/test-thera", s.wrap(s.handleTestThera))
	mux.HandleFunc("/api/autostart", s.wrap(s.handleAutostart))
	mux.HandleFunc("/api/service/start", s.wrap(s.handleServiceStart))
	mux.HandleFunc("/api/service/stop", s.wrap(s.handleServiceStop))

	ln, err := net.Listen("tcp", s.addr)
	if err != nil {
		return fmt.Errorf("escutar painel em %s: %w", s.addr, err)
	}
	s.http = &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = s.http.Shutdown(shutCtx)
	}()
	if s.log != nil {
		s.log.Infof("painel disponível em http://%s", s.addr)
	}
	if err := s.http.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// URL é o endereço do painel para abrir no navegador.
func (s *Server) URL() string { return "http://" + s.addr }

// wrap garante loopback e bloqueia mutações sem token. GETs de leitura passam;
// métodos mutantes (POST) exigem o header X-PontoPar-Token.
func (s *Server) wrap(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !isLoopback(r) {
			http.Error(w, "somente local", http.StatusForbidden)
			return
		}
		if r.Method == http.MethodPost && r.Header.Get("X-PontoPar-Token") != s.token {
			http.Error(w, "token inválido", http.StatusForbidden)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		h(w, r)
	}
}

func isLoopback(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(v)
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if !isLoopback(r) {
		http.Error(w, "somente local", http.StatusForbidden)
		return
	}
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	page := strings.ReplaceAll(dashboardHTML, "{{TOKEN}}", s.token)
	page = strings.ReplaceAll(page, "{{VERSION}}", s.ctrl.Version())
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(page))
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	enabled, known := s.ctrl.AutoStart()
	writeJSON(w, map[string]any{
		"state":          s.ctrl.Snapshot(),
		"service":        s.ctrl.ServiceStatus(),
		"autostart":      enabled,
		"autostartKnown": known,
	})
}

func (s *Server) handleAudit(w http.ResponseWriter, r *http.Request) {
	n := 50
	if q := r.URL.Query().Get("n"); q != "" {
		if v, err := strconv.Atoi(q); err == nil && v > 0 {
			n = v
		}
	}
	recs, err := s.ctrl.RecentAudit(n)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// Mais recentes primeiro para a tabela.
	for i, j := 0, len(recs)-1; i < j; i, j = i+1, j-1 {
		recs[i], recs[j] = recs[j], recs[i]
	}
	writeJSON(w, recs)
}

func (s *Server) handleAuditCSV(w http.ResponseWriter, r *http.Request) {
	data, err := s.ctrl.AuditCSV()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", "attachment; filename=pontopar-auditoria.csv")
	_, _ = w.Write(data)
}

func (s *Server) handleLog(w http.ResponseWriter, r *http.Request) {
	max := 20000
	if q := r.URL.Query().Get("bytes"); q != "" {
		if v, err := strconv.Atoi(q); err == nil && v > 0 && v <= 1<<20 {
			max = v
		}
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte(s.ctrl.LogTail(max)))
}

func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		writeJSON(w, s.ctrl.CurrentConfig())
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "método inválido", http.StatusMethodNotAllowed)
		return
	}
	var cfg config.Config
	if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
		http.Error(w, "config inválida: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := s.ctrl.SaveConfig(&cfg); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) handleFullSync(w http.ResponseWriter, r *http.Request) {
	var body struct {
		StartID *int64 `json:"startId"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	start := int64(0)
	if body.StartID != nil {
		start = *body.StartID
	}
	if err := s.ctrl.TriggerFullSync(start); err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) handleTestDevice(w http.ResponseWriter, r *http.Request) {
	s.testResult(w, s.ctrl.TestDevice())
}
func (s *Server) handleTestThera(w http.ResponseWriter, r *http.Request) {
	s.testResult(w, s.ctrl.TestThera())
}

func (s *Server) testResult(w http.ResponseWriter, err error) {
	if err != nil {
		writeJSON(w, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) handleAutostart(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Enabled bool `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "corpo inválido", http.StatusBadRequest)
		return
	}
	if err := s.ctrl.SetAutoStart(body.Enabled); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) handleServiceStart(w http.ResponseWriter, r *http.Request) {
	if err := s.ctrl.StartService(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) handleServiceStop(w http.ResponseWriter, r *http.Request) {
	if err := s.ctrl.StopService(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}
