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
	"fmt"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/p-lacerda/pontopar-coletor/internal/applog"
	"github.com/p-lacerda/pontopar-coletor/internal/audit"
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

	reload chan struct{}
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
	go func() {
		if err := srv.Serve(ctx); err != nil {
			// Bind falhou (porta ocupada, etc.): loga e segue. A coleta não depende
			// do painel.
			s.log.Errorf("painel web indisponível (%v) — a coleta continua normal", err)
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

func (s *Supervisor) Version() string          { return s.version }
func (s *Supervisor) Snapshot() state.Snapshot { return s.st.Get() }

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
	_, cfg := s.current()
	if cfg != nil {
		return cfg
	}
	// Config inválida/ausente: devolve o que estiver no disco ou os defaults, para
	// o formulário poder ser preenchido e corrigido.
	if loaded, err := config.Load(s.dir); err == nil {
		return loaded
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

func (s *Supervisor) TriggerFullSync(startID int64) error {
	col, _ := s.current()
	if col == nil {
		return fmt.Errorf("coletor não está ativo (config inválida?)")
	}
	if col.FullSyncRunning() {
		return fmt.Errorf("um full-sync já está em andamento")
	}
	ctx := s.runCtx
	if ctx == nil {
		ctx = context.Background()
	}
	go func() {
		// Timeout generoso: um histórico grande pode levar minutos.
		fsCtx, cancel := context.WithTimeout(ctx, 60*time.Minute)
		defer cancel()
		if _, err := col.FullSync(fsCtx, startID, "disparado pelo painel"); err != nil {
			s.log.Errorf("full-sync terminou com erro: %v", err)
		}
	}()
	return nil
}

func (s *Supervisor) TestDevice() error {
	cfg := s.CurrentConfig()
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	dev := idface.New(cfg.DeviceBase(), cfg.Login, cfg.Password, &http.Client{Timeout: 6 * time.Second}, s.log)
	return dev.EnsureSession(ctx)
}

func (s *Supervisor) TestThera() error {
	cfg := s.CurrentConfig()
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	api := thera.New(cfg.TheraDaoURL(), &http.Client{Timeout: 10 * time.Second})
	_, err := api.GetSync(ctx)
	return err
}

func (s *Supervisor) SetAutoStart(enable bool) error { return winservice.SetAutoStart(enable) }
func (s *Supervisor) StartService() error            { return winservice.Start() }
func (s *Supervisor) StopService() error             { return winservice.Stop() }
