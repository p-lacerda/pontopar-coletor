// Comando pontopar-preview serve o PAINEL do coletor com dados de exemplo, em
// qualquer SO, para trabalhar no visual sem Windows, sem relógio e sem Thera:
//
//	go run ./cmd/pontopar-preview -scenario ok        # coletando normalmente
//	go run ./cmd/pontopar-preview -scenario refill    # refill em andamento
//	go run ./cmd/pontopar-preview -scenario offline   # relógio sem conexão
//	go run ./cmd/pontopar-preview -scenario setup     # ainda não instalado
//
// Abre em http://127.0.0.1:47199. Nada aqui fala com aparelho ou nuvem.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/p-lacerda/pontopar-coletor/internal/audit"
	"github.com/p-lacerda/pontopar-coletor/internal/config"
	"github.com/p-lacerda/pontopar-coletor/internal/state"
	"github.com/p-lacerda/pontopar-coletor/internal/webui"
)

type fake struct {
	st       *state.State
	scenario string
	cfg      *config.Config
}

func (f *fake) Mode() string {
	if f.scenario == "setup" {
		return webui.ModeSetup
	}
	return webui.ModeService
}
func (f *fake) Snapshot() state.Snapshot { return f.st.Get() }
func (f *fake) ServiceStatus() string {
	if f.scenario == "setup" {
		return "nao instalado"
	}
	return "rodando"
}
func (f *fake) AutoStart() (bool, bool) { return f.scenario != "setup", f.scenario != "setup" }
func (f *fake) TrayAutoStart() bool     { return f.scenario != "setup" }
func (f *fake) DataDir() string         { return `C:\ProgramData\PontoParColetor` }
func (f *fake) RecentAudit(n int) ([]audit.Record, error) {
	now := time.Now()
	return []audit.Record{
		{Time: now.Add(-50 * time.Hour), Mode: audit.ModeFull, FromID: 0, ToID: 18452, Read: 8120, Forwarded: 8120, Verified: true, DurationMs: 94000, Note: "disparado pelo painel"},
		{Time: now.Add(-26 * time.Hour), Mode: audit.ModeFullPartial, FromID: 0, ToID: 9001, Read: 3300, Forwarded: 3200, Err: "interrompido (retomável)", Note: "disparado pela bandeja"},
		{Time: now.Add(-3 * time.Hour), Mode: audit.ModeFull, FromID: 0, ToID: 18990, DeviceMinID: 10871, RotatedGap: 10870, Read: 8119, Forwarded: 8119, NoRegistration: 14, Verified: true, DurationMs: 81000, Note: "disparado pelo Thera"},
		{Time: now.Add(-9 * time.Minute), Mode: audit.ModeIncremental, FromID: 18987, ToID: 18990, Read: 3, Forwarded: 3, Verified: false},
	}, nil
}
func (f *fake) AuditCSV() ([]byte, error) { return []byte("time,mode\n"), nil }
func (f *fake) LogTail(int) string {
	t := time.Now().Add(-2 * time.Minute)
	l := ""
	for i, m := range []string{
		"Coletor PontoPar iniciado · aparelho=http://192.168.1.111:90 · thera=https://pediuai-api.debita.ai/thera · poll=15s · lote=500",
		"painel disponível em http://127.0.0.1:47114",
		"sincronização concluída: 11 usuários observados",
		"batida encaminhada: id=18988 user=900000004 matricula=0003 time=1762873201",
		"ERRO sincronização do relógio falhou (batidas continuam): context deadline exceeded",
		"batida encaminhada: id=18989 user=900000006 matricula=0004 time=1762873262",
		"batida encaminhada: id=18990 user=900000011 matricula=- time=1762873320",
	} {
		l += t.Add(time.Duration(i)*13*time.Second).Format(time.RFC3339) + " " + m + "\n"
	}
	return l
}
func (f *fake) CurrentConfig() *config.Config     { return f.cfg }
func (f *fake) SaveConfig(c *config.Config) error { f.cfg = c; return nil }
func (f *fake) TriggerFullSync(a, b, c int64, s string) error {
	f.st.FullSyncStart(state.FullSyncInfo{Source: s, StartID: a, FromUnix: b, ToUnix: c})
	return nil
}
func (f *fake) CollectNow() error   { f.st.Tick(); return nil }
func (f *fake) CancelRefill() error { f.st.FullSyncDone("interrompido"); return nil }
func (f *fake) Restart() error      { return nil }
func (f *fake) TestDevice(*config.Config) error {
	if f.scenario == "offline" {
		return errors.New("dial tcp 192.168.1.111:90: connectex: A connection attempt failed")
	}
	return nil
}
func (f *fake) TestThera(*config.Config) error { return nil }
func (f *fake) SetAutoStart(bool) error        { return nil }
func (f *fake) StartService() error            { return nil }
func (f *fake) StopService() error             { return nil }
func (f *fake) Install() error                 { return nil }
func (f *fake) Version() string                { return "v0.12.0" }

func main() {
	scenario := flag.String("scenario", "ok", "ok | refill | offline | setup")
	addr := flag.String("addr", "127.0.0.1:47199", "endereço")
	flag.Parse()

	dir, _ := os.MkdirTemp("", "pontopar-preview-*")
	st := state.New(dir, "v0.12.0")
	cfg := config.Default()
	cfg.DeviceSecret = "segredo-de-exemplo"
	f := &fake{st: st, scenario: *scenario, cfg: cfg}

	st.SetEndpoints(cfg.DeviceBase(), cfg.TheraBase)
	st.SetPoll(15)
	if *scenario != "setup" {
		st.DeviceOK()
		st.TheraOK()
		st.Tick()
		st.SetCursor(18990)
		st.UserSync(11)
		st.Forwarded(132)
		now := time.Now()
		regs := []string{"0003", "0004", "0007", "", "0012", "0003", "0009", "0004"}
		for i := len(regs) - 1; i >= 0; i-- {
			st.RecordPunch(state.Punch{ID: int64(18983 + i), UserID: fmt.Sprintf("9000000%02d", i+3), Registration: regs[i], Time: now.Add(-time.Duration(i*17+2) * time.Minute).Unix(), SentAt: now.Add(-time.Duration(i*17+1) * time.Minute)})
		}
		recs, _ := f.RecentAudit(10)
		st.FullSyncDone("ok", recs[2])
	}
	switch *scenario {
	case "offline":
		st.DeviceFail("dial tcp 192.168.1.111:90: connectex: A connection attempt failed because the connected party did not properly respond after a period of time")
		st.RecordError("load_objects: context deadline exceeded")
	case "refill":
		st.FullSyncStart(state.FullSyncInfo{Source: "thera"})
		st.FullSyncProgress(12450, 4210, 4300)
	case "setup":
		cfg.DeviceSecret = ""
	}
	srv := webui.NewServer(f, *addr, nil)
	fmt.Println("preview em", srv.URL(), "· cenário", *scenario)
	if err := srv.Serve(context.Background()); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
