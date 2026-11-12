// Package webui serve o PAINEL do coletor em 127.0.0.1 (loopback apenas).
//
// O painel é a interface principal do PontoPar Coletor: status ao vivo
// (aparelho, Thera, batidas de hoje, últimas batidas), o botão de REFILL
// (reenviar ao Thera o que o aparelho guarda — tudo, desde uma data, ou um
// período), "Coletar agora", todas as configurações, o "Iniciar com o Windows",
// log ao vivo e a auditoria exportável. O app da bandeja abre este painel numa
// janela própria (Edge/Chrome em modo app).
//
// Roda em dois modos:
//   - "service": DENTRO do serviço do Windows (produção) — o status é o estado
//     real do coletor, não uma cópia.
//   - "setup": servido pelo app da bandeja quando o serviço ainda não está
//     instalado — para preencher a configuração e instalar.
//
// Segurança: escuta só em loopback; recusa Host que não seja localhost (bloqueia
// DNS rebinding — um site não consegue ler a config nem com o IP apontado para
// 127.0.0.1); mutações exigem um token por processo, injetado na página servida
// e gravado em panel.token para a bandeja.
package webui

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/p-lacerda/pontopar-coletor/internal/assets"
	"github.com/p-lacerda/pontopar-coletor/internal/audit"
	"github.com/p-lacerda/pontopar-coletor/internal/config"
	"github.com/p-lacerda/pontopar-coletor/internal/state"
)

// Modos do painel.
const (
	ModeService = "service"
	ModeSetup   = "setup"
)

// TokenHeader é o header que autoriza mutações.
const TokenHeader = "X-PontoPar-Token"

// Logger é a interface mínima de log.
type Logger interface {
	Infof(format string, args ...any)
	Errorf(format string, args ...any)
}

// Controller é a ponte entre o painel e o coletor/serviço. É implementado pelo
// supervisor (internal/app) no serviço e pelo modo setup da bandeja.
type Controller interface {
	// Mode é ModeService ou ModeSetup.
	Mode() string
	// Snapshot é o estado vivo do coletor.
	Snapshot() state.Snapshot
	// ServiceStatus é o estado do serviço Windows ("rodando", "parado", "nao instalado"...).
	ServiceStatus() string
	// AutoStart diz se o "Iniciar com o Windows" está ligado e se foi possível
	// consultar (known=false quando o serviço não está instalado / fora do Windows).
	AutoStart() (enabled bool, known bool)
	// TrayAutoStart diz se o app da bandeja abre no logon.
	TrayAutoStart() bool
	// DataDir é a pasta de config/cursor/log.
	DataDir() string
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
	// TriggerFullSync dispara o refill em segundo plano. startID<0 retoma; 0 é
	// desde o início. fromUnix/toUnix (epoch s; 0 = sem limite) recortam o
	// período. source diz quem pediu (painel, bandeja).
	TriggerFullSync(startID, fromUnix, toUnix int64, source string) error
	// CollectNow acorda o coletor para coletar já.
	CollectNow() error
	// CancelRefill interrompe o refill em andamento (fica retomável).
	CancelRefill() error
	// Restart recarrega o coletor (relê a config, refaz as sessões).
	Restart() error
	// TestDevice/TestThera checam conectividade sob demanda com a config dada
	// (a que está no formulário, ainda não salva) ou, se nil, a em uso.
	TestDevice(*config.Config) error
	TestThera(*config.Config) error
	// SetAutoStart liga/desliga o início com o Windows.
	SetAutoStart(enable bool) error
	// StartService/StopService controlam o serviço.
	StartService() error
	StopService() error
	// Install instala o serviço (modo setup; pede UAC).
	Install() error
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
	now   func() time.Time
}

// NewServer cria o servidor em addr (ex.: "127.0.0.1:47114"). O token é gerado
// agora e injetado na página.
func NewServer(ctrl Controller, addr string, log Logger) *Server {
	return &Server{ctrl: ctrl, log: log, addr: addr, token: newToken(), now: time.Now}
}

func newToken() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "pontopar-local"
	}
	return hex.EncodeToString(b)
}

// Token é o token de mutação deste servidor (a bandeja o lê de panel.token).
func (s *Server) Token() string { return s.token }

// Handler monta o roteamento (exposto para testes).
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleIndex)
	mux.HandleFunc("/logo.png", s.asset(assets.Glyph256))
	mux.HandleFunc("/theramais.png", s.asset(assets.TheraMais256))
	mux.HandleFunc("/favicon.png", s.asset(assets.Glyph64))
	mux.HandleFunc("/favicon.ico", s.asset(assets.Glyph32))
	mux.HandleFunc("/api/status", s.wrap(s.handleStatus))
	mux.HandleFunc("/api/audit", s.wrap(s.handleAudit))
	mux.HandleFunc("/api/audit.csv", s.wrap(s.handleAuditCSV))
	mux.HandleFunc("/api/log", s.wrap(s.handleLog))
	mux.HandleFunc("/api/config", s.wrap(s.handleConfig))
	mux.HandleFunc("/api/refill", s.wrap(s.post(s.handleRefill)))
	mux.HandleFunc("/api/fullsync", s.wrap(s.post(s.handleFullSync))) // compat (painel antigo)
	mux.HandleFunc("/api/collect-now", s.wrap(s.post(s.handleCollectNow)))
	mux.HandleFunc("/api/refill/cancel", s.wrap(s.post(func(w http.ResponseWriter, r *http.Request) {
		s.okOr(w, s.ctrl.CancelRefill(), http.StatusConflict)
	})))
	mux.HandleFunc("/api/restart", s.wrap(s.post(s.handleRestart)))
	mux.HandleFunc("/api/test-device", s.wrap(s.post(s.handleTestDevice)))
	mux.HandleFunc("/api/test-thera", s.wrap(s.post(s.handleTestThera)))
	mux.HandleFunc("/api/autostart", s.wrap(s.post(s.handleAutostart)))
	mux.HandleFunc("/api/service/start", s.wrap(s.post(s.handleServiceStart)))
	mux.HandleFunc("/api/service/stop", s.wrap(s.post(s.handleServiceStop)))
	mux.HandleFunc("/api/install", s.wrap(s.post(s.handleInstall)))
	return mux
}

// Listen faz o bind (separado de Serve para o chamador poder tentar de novo
// quando a porta estiver ocupada — ex.: a bandeja liberando o modo setup).
func (s *Server) Listen() (net.Listener, error) {
	ln, err := net.Listen("tcp", s.addr)
	if err != nil {
		return nil, fmt.Errorf("escutar painel em %s: %w", s.addr, err)
	}
	return ln, nil
}

// Serve escuta e serve até ctx ser cancelado. Devolve erro de bind (o chamador
// loga e segue; o painel NUNCA pode derrubar a coleta).
func (s *Server) Serve(ctx context.Context) error {
	ln, err := s.Listen()
	if err != nil {
		return err
	}
	return s.ServeListener(ctx, ln)
}

// ServeListener serve num listener já aberto até ctx ser cancelado.
func (s *Server) ServeListener(ctx context.Context, ln net.Listener) error {
	s.http = &http.Server{Handler: s.Handler(), ReadHeaderTimeout: 5 * time.Second}
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

// wrap garante loopback + Host local e bloqueia mutações sem token.
func (s *Server) wrap(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !isLoopback(r) || !isLocalHost(r.Host) {
			http.Error(w, "somente local", http.StatusForbidden)
			return
		}
		if r.Method == http.MethodPost && r.Header.Get(TokenHeader) != s.token {
			http.Error(w, "token inválido — recarregue o painel", http.StatusForbidden)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		h(w, r)
	}
}

// post restringe o handler a POST.
func (s *Server) post(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "use POST", http.StatusMethodNotAllowed)
			return
		}
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

// isLocalHost aceita só Host de loopback (127.x, ::1, localhost). Um Host de
// outro domínio é o sinal de DNS rebinding.
func isLocalHost(hostport string) bool {
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	host = strings.Trim(strings.ToLower(host), "[]")
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(v)
}

func (s *Server) asset(data []byte) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("Cache-Control", "public, max-age=86400")
		_, _ = w.Write(data)
	}
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if !isLoopback(r) || !isLocalHost(r.Host) {
		http.Error(w, "somente local", http.StatusForbidden)
		return
	}
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	page := strings.ReplaceAll(dashboardHTML, "{{TOKEN}}", s.token)
	page = strings.ReplaceAll(page, "{{VERSION}}", s.ctrl.Version())
	page = strings.ReplaceAll(page, "{{MODE}}", s.ctrl.Mode())
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Frame-Options", "DENY")
	_, _ = w.Write([]byte(page))
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	enabled, known := s.ctrl.AutoStart()
	cfg := s.ctrl.CurrentConfig()
	svc := s.ctrl.ServiceStatus()
	writeJSON(w, map[string]any{
		"mode":             s.ctrl.Mode(),
		"version":          s.ctrl.Version(),
		"state":            s.ctrl.Snapshot(),
		"service":          svc,
		"serviceInstalled": svc != "nao instalado" && svc != "desconhecido" && svc != "",
		"autostart":        enabled,
		"autostartKnown":   known,
		"trayAutostart":    s.ctrl.TrayAutoStart(),
		"dataDir":          s.ctrl.DataDir(),
		"deviceIp":         cfg.DeviceIp,
		"devicePort":       cfg.Port(),
		"deviceId":         strconv.FormatInt(cfg.DeviceIdInt(), 10),
		"theraBase":        cfg.TheraBase,
		"configValid":      cfg.Validate() == nil,
	})
}

func (s *Server) handleAudit(w http.ResponseWriter, r *http.Request) {
	n := 50
	if q := r.URL.Query().Get("n"); q != "" {
		if v, err := strconv.Atoi(q); err == nil && v > 0 {
			n = v
		}
	}
	mode := r.URL.Query().Get("mode")
	window := n
	if mode != "" {
		window = 0 // filtra sobre TUDO: os refills não podem sumir entre os incrementais
	}
	recs, err := s.ctrl.RecentAudit(window)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if mode != "" {
		kept := recs[:0]
		for _, rec := range recs {
			if strings.HasPrefix(rec.Mode, mode) {
				kept = append(kept, rec)
			}
		}
		recs = kept
		if len(recs) > n {
			recs = recs[len(recs)-n:]
		}
	}
	// Mais recentes primeiro para a tabela.
	for i, j := 0, len(recs)-1; i < j; i, j = i+1, j-1 {
		recs[i], recs[j] = recs[j], recs[i]
	}
	if recs == nil {
		recs = []audit.Record{}
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

// RefillBody é o corpo de POST /api/refill.
//
//	mode: "all"    → tudo o que o aparelho guarda (desde o id 0)
//	      "since"  → desde a data `from` (inclusive) até agora
//	      "period" → de `from` a `to` (inclusive, datas locais)
//	      "days"   → últimos `days` dias (hoje conta como 1)
//	      "resume" → retoma o refill interrompido
type RefillBody struct {
	Mode   string `json:"mode"`
	From   string `json:"from"` // AAAA-MM-DD
	To     string `json:"to"`   // AAAA-MM-DD
	Days   int    `json:"days"`
	Source string `json:"source"`
}

// Resolve converte o corpo em (startID, fromUnix, toUnix).
func (b RefillBody) Resolve(now time.Time) (startID, fromUnix, toUnix int64, err error) {
	switch strings.ToLower(strings.TrimSpace(b.Mode)) {
	case "", "all":
		return 0, 0, 0, nil
	case "resume":
		return -1, 0, 0, nil
	case "days":
		if b.Days < 1 || b.Days > 3650 {
			return 0, 0, 0, fmt.Errorf("quantidade de dias inválida")
		}
		day := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location()).AddDate(0, 0, -(b.Days - 1))
		return 0, day.Unix(), 0, nil
	case "since":
		from, err := dayStartUnix(b.From)
		if err != nil || from == 0 {
			return 0, 0, 0, fmt.Errorf("informe a data inicial (AAAA-MM-DD)")
		}
		return 0, from, 0, nil
	case "period":
		from, err := dayStartUnix(b.From)
		if err != nil {
			return 0, 0, 0, fmt.Errorf("data inicial inválida (use AAAA-MM-DD)")
		}
		to, err := dayEndUnix(b.To)
		if err != nil {
			return 0, 0, 0, fmt.Errorf("data final inválida (use AAAA-MM-DD)")
		}
		if from == 0 && to == 0 {
			return 0, 0, 0, fmt.Errorf("informe ao menos uma data do período")
		}
		if from > 0 && to > 0 && to < from {
			return 0, 0, 0, fmt.Errorf("a data final é anterior à inicial")
		}
		return 0, from, to, nil
	default:
		return 0, 0, 0, fmt.Errorf("modo de refill desconhecido: %q", b.Mode)
	}
}

func (s *Server) handleRefill(w http.ResponseWriter, r *http.Request) {
	var body RefillBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "corpo inválido", http.StatusBadRequest)
		return
	}
	start, from, to, err := body.Resolve(s.now())
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	source := strings.TrimSpace(body.Source)
	if source == "" {
		source = "painel"
	}
	if err := s.ctrl.TriggerFullSync(start, from, to, source); err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	writeJSON(w, map[string]any{"ok": true, "startId": start, "fromUnix": from, "toUnix": to})
}

// handleFullSync é o endpoint do painel antigo (startId + datas).
func (s *Server) handleFullSync(w http.ResponseWriter, r *http.Request) {
	var body struct {
		StartID  *int64 `json:"startId"`
		FromDate string `json:"fromDate"`
		ToDate   string `json:"toDate"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	start := int64(0)
	if body.StartID != nil {
		start = *body.StartID
	}
	fromUnix, err := dayStartUnix(body.FromDate)
	if err != nil {
		http.Error(w, "data inicial inválida (use AAAA-MM-DD)", http.StatusBadRequest)
		return
	}
	toUnix, err := dayEndUnix(body.ToDate)
	if err != nil {
		http.Error(w, "data final inválida (use AAAA-MM-DD)", http.StatusBadRequest)
		return
	}
	if fromUnix > 0 && toUnix > 0 && toUnix < fromUnix {
		http.Error(w, "data final anterior à inicial", http.StatusBadRequest)
		return
	}
	if err := s.ctrl.TriggerFullSync(start, fromUnix, toUnix, "painel"); err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

// dayStartUnix converte "AAAA-MM-DD" no INÍCIO do dia (00:00 local) para epoch em
// segundos. Vazio => 0 (sem limite).
func dayStartUnix(date string) (int64, error) {
	date = strings.TrimSpace(date)
	if date == "" {
		return 0, nil
	}
	t, err := time.ParseInLocation("2006-01-02", date, time.Local)
	if err != nil {
		return 0, err
	}
	return t.Unix(), nil
}

// dayEndUnix converte "AAAA-MM-DD" no FIM do dia (23:59:59 local, inclusivo) para
// epoch em segundos. Vazio => 0 (sem limite).
func dayEndUnix(date string) (int64, error) {
	date = strings.TrimSpace(date)
	if date == "" {
		return 0, nil
	}
	t, err := time.ParseInLocation("2006-01-02", date, time.Local)
	if err != nil {
		return 0, err
	}
	return t.AddDate(0, 0, 1).Add(-time.Second).Unix(), nil
}

func (s *Server) handleCollectNow(w http.ResponseWriter, r *http.Request) {
	s.okOr(w, s.ctrl.CollectNow(), http.StatusConflict)
}

func (s *Server) handleRestart(w http.ResponseWriter, r *http.Request) {
	s.okOr(w, s.ctrl.Restart(), http.StatusConflict)
}

func (s *Server) handleTestDevice(w http.ResponseWriter, r *http.Request) {
	cfg, err := formConfig(r)
	if err != nil {
		s.testResult(w, err)
		return
	}
	s.testResult(w, s.ctrl.TestDevice(cfg))
}

func (s *Server) handleTestThera(w http.ResponseWriter, r *http.Request) {
	cfg, err := formConfig(r)
	if err != nil {
		s.testResult(w, err)
		return
	}
	s.testResult(w, s.ctrl.TestThera(cfg))
}

// formConfig lê a config do formulário enviada no corpo do teste. Corpo vazio =
// testa a config em uso. Corpo inválido é ERRO (antes caía calado na config
// salva e o usuário achava que tinha testado o que digitou).
func formConfig(r *http.Request) (*config.Config, error) {
	if r.Body == nil {
		return nil, nil
	}
	data, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if len(strings.TrimSpace(string(data))) == 0 || strings.TrimSpace(string(data)) == "null" {
		return nil, nil
	}
	var c config.Config
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("os dados digitados são inválidos: %v", err)
	}
	return &c, nil
}

func (s *Server) testResult(w http.ResponseWriter, err error) {
	if err != nil {
		writeJSON(w, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) okOr(w http.ResponseWriter, err error, code int) {
	if err != nil {
		http.Error(w, err.Error(), code)
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
	s.okOr(w, s.ctrl.SetAutoStart(body.Enabled), http.StatusInternalServerError)
}

func (s *Server) handleServiceStart(w http.ResponseWriter, r *http.Request) {
	s.okOr(w, s.ctrl.StartService(), http.StatusInternalServerError)
}

func (s *Server) handleServiceStop(w http.ResponseWriter, r *http.Request) {
	s.okOr(w, s.ctrl.StopService(), http.StatusInternalServerError)
}

func (s *Server) handleInstall(w http.ResponseWriter, r *http.Request) {
	s.okOr(w, s.ctrl.Install(), http.StatusInternalServerError)
}
