package desktop

import (
	"bytes"
	"errors"
	"os"

	"github.com/p-lacerda/pontopar-coletor/internal/app"
	"github.com/p-lacerda/pontopar-coletor/internal/applog"
	"github.com/p-lacerda/pontopar-coletor/internal/audit"
	"github.com/p-lacerda/pontopar-coletor/internal/autorun"
	"github.com/p-lacerda/pontopar-coletor/internal/config"
	"github.com/p-lacerda/pontopar-coletor/internal/state"
	"github.com/p-lacerda/pontopar-coletor/internal/webui"
	"github.com/p-lacerda/pontopar-coletor/internal/winservice"
)

var errNotInstalled = errors.New("instale o coletor primeiro (botão “Instalar e iniciar”)")

// setupCtrl é o painel no MODO SETUP: servido pelo app da bandeja quando o
// serviço ainda não está instalado. Deixa preencher/testar/salvar a config ao
// lado do executável e instalar (com UAC). Não coleta nada — coleta é só no
// serviço, para nunca existirem dois coletores ao mesmo tempo.
type setupCtrl struct {
	dir     string
	version string
	install func() error
}

func (c *setupCtrl) Mode() string    { return webui.ModeSetup }
func (c *setupCtrl) Version() string { return c.version }
func (c *setupCtrl) DataDir() string { return c.dir }
func (c *setupCtrl) Snapshot() state.Snapshot {
	return state.Snapshot{Version: c.version}
}
func (c *setupCtrl) ServiceStatus() string {
	st, err := winservice.Status()
	if err != nil {
		return "desconhecido"
	}
	return st
}
func (c *setupCtrl) AutoStart() (bool, bool) { return false, false }
func (c *setupCtrl) TrayAutoStart() bool     { return autorun.Enabled() }
func (c *setupCtrl) RecentAudit(n int) ([]audit.Record, error) {
	if _, err := os.Stat(audit.New(c.dir).Path()); err != nil {
		return nil, nil
	}
	return audit.New(c.dir).Recent(n)
}
func (c *setupCtrl) AuditCSV() ([]byte, error) {
	recs, _ := c.RecentAudit(1 << 20)
	var buf bytes.Buffer
	err := audit.ExportCSV(&buf, recs)
	return buf.Bytes(), err
}
func (c *setupCtrl) LogTail(maxBytes int) string {
	data, err := os.ReadFile(applog.DefaultPath(c.dir))
	if err != nil {
		return "O coletor ainda não rodou neste computador."
	}
	if len(data) > maxBytes {
		data = data[len(data)-maxBytes:]
	}
	return string(data)
}
func (c *setupCtrl) CurrentConfig() *config.Config {
	if cfg, err := config.Load(c.dir); err == nil {
		return cfg
	}
	return config.Default()
}
func (c *setupCtrl) SaveConfig(cfg *config.Config) error { return config.Save(c.dir, cfg) }
func (c *setupCtrl) TriggerFullSync(int64, int64, int64, string) error {
	return errNotInstalled
}
func (c *setupCtrl) CollectNow() error   { return errNotInstalled }
func (c *setupCtrl) CancelRefill() error { return errNotInstalled }
func (c *setupCtrl) Restart() error      { return errNotInstalled }
func (c *setupCtrl) TestDevice(cfg *config.Config) error {
	if cfg == nil {
		cfg = c.CurrentConfig()
	}
	return app.TestDeviceConn(cfg, nil)
}
func (c *setupCtrl) TestThera(cfg *config.Config) error {
	if cfg == nil {
		cfg = c.CurrentConfig()
	}
	return app.TestTheraConn(cfg)
}
func (c *setupCtrl) SetAutoStart(bool) error { return errNotInstalled }
func (c *setupCtrl) StartService() error     { return ignoreRunning(winservice.Start()) }

func ignoreRunning(err error) error {
	if errors.Is(err, winservice.ErrAlreadyRunning) {
		return nil
	}
	return err
}
func (c *setupCtrl) StopService() error { return errNotInstalled }
func (c *setupCtrl) Install() error {
	if err := c.CurrentConfig().Validate(); err != nil {
		return errors.New("preencha e salve uma configuração válida antes de instalar: " + err.Error())
	}
	if c.install == nil {
		return errors.New("instalação indisponível neste sistema")
	}
	return c.install()
}
