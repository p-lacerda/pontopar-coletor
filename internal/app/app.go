// Package app é o SUPERVISOR do coletor: carrega a config, constrói e roda o
// coletor, sobe o painel web (uma vez) e aplica RECARGA a quente quando a config
// muda (salva pelo painel) — sem precisar reinstalar o serviço. Também implementa
// a ponte (webui.Controller) entre o painel e o coletor/serviço.
//
// Desenho de robustez:
//   - O painel roda DENTRO deste processo (o serviço), então mostra o estado real.
//   - Uma falha do painel (bind, etc.) é logada e IGNORADA: a coleta nunca depende
//     dele.
//   - Se a config estiver inválida no arranque, o coletor não sobe, mas o painel
//     SOBE assim mesmo (porta default), para o operador corrigir e salvar — e o
//     supervisor recarrega sozinho.
package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/p-lacerda/pontopar-coletor/internal/applog"
	"github.com/p-lacerda/pontopar-coletor/internal/audit"
	"github.com/p-lacerda/pontopar-coletor/internal/autorun"
	"github.com/p-lacerda/pontopar-coletor/internal/collector"
	"github.com/p-lacerda/pontopar-coletor/internal/config"
	"github.com/p-lacerda/pontopar-coletor/internal/idface"
	"github.com/p-lacerda/pontopar-coletor/internal/state"
	"github.com/p-lacerda/pontopar-coletor/internal/thera"
	"github.com/p-lacerda/pontopar-coletor/internal/updater"
	"github.com/p-lacerda/pontopar-coletor/internal/webui"
	"github.com/p-lacerda/pontopar-coletor/internal/winservice"
)

// Supervisor gerencia o ciclo de vida do coletor e do painel.
type Supervisor struct {
	dir         string
	version     string
	interactive bool
	log         *applog.Logger
	st          *state.State
	au          *audit.Log

	mu     sync.Mutex
	cfg    *config.Config
	col    *collector.Collector
	runCtx context.Context

	reload        chan struct{}
	reloadPending atomic.Bool // config salva esperando o refill terminar
}

// NewSupervisor cria o supervisor. interactive=true no modo "run" (foreground):
// um update NÃO reinicia o processo sozinho (só avisa).
func NewSupervisor(dir, version string, interactive bool, log *applog.Logger, st *state.State, au *audit.Log) *Supervisor {
	return &Supervisor{
		dir: dir, version: version, interactive: interactive,
		log: log, st: st, au: au, reload: make(chan struct{}, 1),
	}
}

// Run roda até ctx ser cancelado. Sobe o painel uma vez e mantém o coletor vivo,
// reconstruindo-o quando a config muda.
func (s *Supervisor) Run(ctx context.Context) {
	s.runCtx = ctx

	// Sobe o painel (uma vez) na porta da config (ou default se a config falhar).
	s.startPanel(ctx)

	for {
		cfg, err := config.Load(s.dir)
		if err != nil {
			s.log.Errorf("configuração inválida (%v) — coleta pausada; corrija pelo painel e salve", err)
			s.setCurrent(nil, nil)
			select {
			case <-ctx.Done():
				return
			case <-s.reload:
				continue
			case <-time.After(30 * time.Second):
				continue
			}
		}

		col := collector.New(cfg, s.dir, s.log, s.st, s.au)
		s.wireUpdater(col, cfg)
		s.setCurrent(col, cfg)

		innerCtx, cancel := context.WithCancel(ctx)
		done := make(chan struct{})
		go func() {
			col.Run(innerCtx)
			close(done)
		}()

		select {
		case <-ctx.Done():
			cancel()
			<-done
			return
		case <-s.reload:
			// Nunca interrompe um refill para recarregar: a config já está salva
			// e passa a valer assim que ele terminar.
			if col.FullSyncRunning() {
				s.log.Infof("configuração salva; recarga adiada até o refill terminar")
				s.reloadPending.Store(true)
				for col.FullSyncRunning() {
					select {
					case <-ctx.Done():
						cancel()
						<-done
						return
					case <-time.After(2 * time.Second):
					}
				}
			}
			s.reloadPending.Store(false)
			s.log.Infof("recarregando configuração do coletor…")
			cancel()
			<-done
			// loop reconstrói com a nova config
		case <-done:
			// O coletor saiu sozinho. Em serviço, só acontece após um auto-update
			// (onUpdated já encerrou o processo). Em interativo, após update sem
			// restart: encerra o supervisor.
			cancel()
			return
		}
	}
}

// PanelFile é o arquivo (na pasta de dados) onde o serviço publica o endereço
// REAL do painel e o token de mutação, para o app da bandeja. Mesma confiança do
// config.json. O endereço vem daqui (e não do config) porque uma troca de porta
// no config só vale quando o serviço reinicia.
const PanelFile = "panel.json"

// PanelInfo é o conteúdo de panel.json.
type PanelInfo struct {
	Addr  string `json:"addr"`
	Token string `json:"token"`
}

// ReadPanelInfo lê panel.json da pasta de dados (zero se ausente).
func ReadPanelInfo(dir string) PanelInfo {
	var pi PanelInfo
	if data, err := os.ReadFile(filepath.Join(dir, PanelFile)); err == nil {
		_ = json.Unmarshal(data, &pi)
	}
	return pi
}

func (s *Supervisor) startPanel(ctx context.Context) {
	addr := "127.0.0.1:47114"
	enabled := true
	if cfg, err := config.Load(s.dir); err == nil {
		enabled = cfg.UIEnabled()
		addr = fmt.Sprintf("%s:%d", cfg.UIBind(), cfg.UIPort())
	}
	if !enabled {
		s.log.Infof("painel web desabilitado na config (ui.enabled=false)")
		return
	}
	srv := webui.NewServer(s, addr, s.log)
	if data, err := json.Marshal(PanelInfo{Addr: addr, Token: srv.Token()}); err == nil {
		_ = os.WriteFile(filepath.Join(s.dir, PanelFile), data, 0o644)
	}
	go func() {
		// O bind pode falhar por alguns segundos (ex.: o app da bandeja ainda
		// está liberando o painel do modo setup logo após instalar). Tenta de
		// novo até conseguir; a coleta nunca depende do painel.
		warned := false
		for {
			ln, err := srv.Listen()
			if err == nil {
				if warned {
					s.log.Infof("painel web disponível novamente")
				}
				if err := srv.ServeListener(ctx, ln); err != nil {
					s.log.Errorf("painel web parou (%v) — a coleta continua normal", err)
				}
				return
			}
			if !warned {
				s.log.Errorf("painel web indisponível (%v) — tentando de novo; a coleta continua normal", err)
				warned = true
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(5 * time.Second):
			}
		}
	}()
}

func (s *Supervisor) wireUpdater(col *collector.Collector, cfg *config.Config) {
	up, err := updater.New(cfg, s.version, s.log)
	if err != nil {
		s.log.Errorf("aviso: auto-update não pôde iniciar: %v", err)
		return
	}
	if up == nil {
		return
	}
	every := time.Duration(cfg.UpdateCheckHours()) * time.Hour
	interactive := s.interactive
	onUpdated := func(newVersion string) {
		if interactive {
			s.log.Infof("nova versão %s baixada ao lado do exe; reinicie o processo para aplicar", newVersion)
			return
		}
		s.log.Close()
		up.RestartForUpdate()
	}
	col.SetUpdater(up, every, onUpdated)
	s.log.Infof("auto-update habilitado: repo=%s a cada %s", cfg.Update.Repo, every)
}

func (s *Supervisor) setCurrent(col *collector.Collector, cfg *config.Config) {
	s.mu.Lock()
	s.col = col
	s.cfg = cfg
	s.mu.Unlock()
}

func (s *Supervisor) current() (*collector.Collector, *config.Config) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.col, s.cfg
}

func (s *Supervisor) signalReload() {
	select {
	case s.reload <- struct{}{}:
	default:
	}
}

// ---- webui.Controller ----

func (s *Supervisor) Mode() string    { return webui.ModeService }
func (s *Supervisor) Version() string { return s.version }
func (s *Supervisor) Snapshot() state.Snapshot {
	snap := s.st.Get()
	if col, _ := s.current(); col != nil {
		snap.RefillResumable = !snap.FullSyncRunning && col.ResumableRefill()
	}
	snap.ReloadPending = s.reloadPending.Load()
	return snap
}
func (s *Supervisor) DataDir() string     { return s.dir }
func (s *Supervisor) TrayAutoStart() bool { return autorun.Enabled() }
func (s *Supervisor) Install() error      { return fmt.Errorf("o coletor já está instalado e rodando") }

// CollectNow acorda o coletor para coletar imediatamente.
func (s *Supervisor) CollectNow() error {
	col, _ := s.current()
	if col == nil {
		return fmt.Errorf("coletor não está ativo (configuração inválida?)")
	}
	col.CollectNow()
	return nil
}

// CancelRefill interrompe o refill em andamento (retomável).
func (s *Supervisor) CancelRefill() error {
	col, _ := s.current()
	if col == nil || !col.CancelRefill() {
		return fmt.Errorf("não há refill em andamento")
	}
	return nil
}

// Restart recarrega o coletor (relê a config e refaz as sessões).
func (s *Supervisor) Restart() error {
	if col, _ := s.current(); col != nil && col.FullSyncRunning() {
		return fmt.Errorf("há um refill em andamento — reinicie a coleta depois que ele terminar")
	}
	s.signalReload()
	return nil
}

func (s *Supervisor) ServiceStatus() string {
	st, err := winservice.Status()
	if err != nil {
		return "desconhecido"
	}
	return st
}

func (s *Supervisor) AutoStart() (bool, bool) {
	enabled, err := winservice.IsAutoStart()
	if err != nil {
		return false, false
	}
	return enabled, true
}

func (s *Supervisor) RecentAudit(n int) ([]audit.Record, error) { return s.au.Recent(n) }

func (s *Supervisor) AuditCSV() ([]byte, error) {
	recs, err := audit.ReadAll(s.au.Path())
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	if err := audit.ExportCSV(&buf, recs); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func (s *Supervisor) LogTail(maxBytes int) string {
	path := applog.DefaultPath(s.dir)
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return ""
	}
	start := int64(0)
	if fi.Size() > int64(maxBytes) {
		start = fi.Size() - int64(maxBytes)
	}
	if _, err := f.Seek(start, 0); err != nil {
		return ""
	}
	data := make([]byte, fi.Size()-start)
	n, _ := f.Read(data)
	return string(data[:n])
}

func (s *Supervisor) CurrentConfig() *config.Config {
	// O DISCO primeiro: com a recarga adiada (refill rodando), a config em memória
	// é a antiga — mostrá-la no formulário e salvar de novo desfaria a alteração.
	if loaded, err := config.Load(s.dir); err == nil {
		return loaded
	}
	if _, cfg := s.current(); cfg != nil {
		return cfg
	}
	return config.Default()
}

func (s *Supervisor) SaveConfig(cfg *config.Config) error {
	if err := config.Save(s.dir, cfg); err != nil {
		return err
	}
	s.signalReload()
	return nil
}

// TriggerFullSync dispara o full-sync em segundo plano. startID<0 retoma; 0 começa
// do início. fromUnix/toUnix (epoch em segundos; 0 = sem limite) recortam o
// período — se ambos 0, é o histórico total.
func (s *Supervisor) TriggerFullSync(startID, fromUnix, toUnix int64, source string) error {
	col, _ := s.current()
	if col == nil {
		return fmt.Errorf("coletor não está ativo (config inválida?)")
	}
	if source == "" {
		source = collector.SourcePanel
	}
	req := collector.RefillRequest{StartID: startID, FromUnix: fromUnix, ToUnix: toUnix, Source: source}
	if err := col.StartRefill(req); err != nil {
		if errors.Is(err, collector.ErrRefillRunning) {
			return fmt.Errorf("um refill já está em andamento — acompanhe o progresso no painel")
		}
		return err
	}
	s.log.Infof("refill pedido pelo %s (startId=%d, período %d..%d)", source, startID, fromUnix, toUnix)
	return nil
}

// TestDevice/TestThera testam a config dada (a do formulário) ou, sem ela, a em uso.
func (s *Supervisor) TestDevice(cfg *config.Config) error {
	if cfg == nil {
		cfg = s.CurrentConfig()
	}
	return TestDeviceConn(cfg, s.log)
}

func (s *Supervisor) TestThera(cfg *config.Config) error {
	if cfg == nil {
		cfg = s.CurrentConfig()
	}
	return TestTheraConn(cfg)
}

// TestDeviceConn faz login no relógio com a config dada (teste sob demanda).
func TestDeviceConn(cfg *config.Config, log idface.Logger) error {
	if log == nil {
		log = nopLogger{}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	dev := idface.New(cfg.DeviceBase(), cfg.Login, cfg.Password, &http.Client{Timeout: 6 * time.Second}, log)
	return dev.EnsureSession(ctx)
}

// TestTheraConn busca o manifesto do Thera com a config dada (valida endereço+segredo).
func TestTheraConn(cfg *config.Config) error {
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	api := thera.New(cfg.TheraDaoURL(), &http.Client{Timeout: 10 * time.Second})
	_, err := api.GetSync(ctx)
	return err
}

type nopLogger struct{}

func (nopLogger) Infof(string, ...any)  {}
func (nopLogger) Errorf(string, ...any) {}

func (s *Supervisor) SetAutoStart(enable bool) error { return winservice.SetAutoStart(enable) }
func (s *Supervisor) StartService() error {
	if err := winservice.Start(); err != nil && !errors.Is(err, winservice.ErrAlreadyRunning) {
		return err
	}
	return nil
}
func (s *Supervisor) StopService() error { return winservice.Stop() }
