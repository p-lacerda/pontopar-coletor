//go:build windows

// Package desktop é o APP do PontoPar Coletor para quem está na frente do PC:
// um ícone na bandeja do Windows (com o status em cores) e o painel numa janela
// própria (Edge/Chrome em modo app — o mesmo painel estilizado servido pelo
// serviço).
//
// Ele NÃO coleta: quem coleta é o serviço do Windows (sessão 0, sobe no boot
// sem ninguém logado). O app só mostra, avisa e comanda:
//   - abre sozinho no logon (HKLM\...\Run, gravado pelo instalador);
//   - se o serviço estiver parado, tenta subi-lo (a instalação dá a usuários
//     interativos o direito de iniciar o serviço) e, sem permissão, pede UAC;
//   - menu com Coletar agora e Refill (tudo / hoje / 7 dias / período);
//   - notifica quando o relógio cai/volta e quando um refill termina;
//   - se o coletor nem está instalado, serve o painel em "modo setup" para
//     configurar e instalar.
package desktop

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"github.com/lxn/walk"
	"github.com/lxn/win"
	"github.com/p-lacerda/pontopar-coletor/internal/app"
	"github.com/p-lacerda/pontopar-coletor/internal/audit"
	"github.com/p-lacerda/pontopar-coletor/internal/config"
	"github.com/p-lacerda/pontopar-coletor/internal/setup"
	"github.com/p-lacerda/pontopar-coletor/internal/webui"
	"github.com/p-lacerda/pontopar-coletor/internal/winservice"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

const (
	title        = "PontoPar Coletor"
	panelTitle   = "PontoPar Coletor" // <title> do painel: é como achamos a janela já aberta
	hostTitle    = "PontoPar Coletor · bandeja"
	mutexName    = `Local\PontoParColetorTray`
	pollInterval = 3 * time.Second
	offlineGrace = 2 * time.Minute  // relógio fora por mais que isso → notificação
	restartEvery = 60 * time.Second // serviço parado com auto-start ligado → religa
	// stoppedGrace: só religa se ficou parado por mais que isso. O recovery do
	// Windows (e o restart de um auto-update) religam em ~5s; sem esta espera a
	// bandeja "religava" junto e avisava à toa.
	stoppedGrace = 10 * time.Second
)

// LogStartupError registra um erro do app da bandeja em
// %LocalAppData%\PontoParColetor\bandeja.log (sem janela: no logon, uma caixa
// de erro na cara do usuário seria pior que o problema).
func LogStartupError(err error) {
	base, e := os.UserCacheDir()
	if e != nil {
		return
	}
	dir := filepath.Join(base, "PontoParColetor")
	_ = os.MkdirAll(dir, 0o755)
	f, e := os.OpenFile(filepath.Join(dir, "bandeja.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if e != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "%s %v\n", time.Now().Format(time.RFC3339), err)
}

// ShowStartupError mostra um erro mesmo quando o app não chegou a abrir.
func ShowStartupError(err error) {
	msgBox(title, "O PontoPar não conseguiu abrir.\n\n"+err.Error(), win.MB_OK|win.MB_ICONERROR)
}

// Run abre o app. minimized=true (logon do Windows) só põe o ícone na bandeja;
// false (atalho/duplo clique) também abre o painel.
func Run(version string, minimized bool) error {
	mutex, already := singleInstance()
	if already {
		// Já existe um app na bandeja: só traz o painel para a frente.
		if !minimized {
			openPanel(panelURL(loadPort()), "")
		}
		return nil
	}
	t := &tray{version: version, started: time.Now(), mutex: mutex, atLogon: minimized}
	return t.run(minimized)
}

type tray struct {
	version string
	started time.Time
	mutex   windows.Handle

	mw *walk.MainWindow
	ni *walk.NotifyIcon

	icons     map[string]*walk.Icon
	statusAct *walk.Action
	openAct   *walk.Action
	collect   *walk.Action
	refillMn  *walk.Action
	startAct  *walk.Action
	autoAct   *walk.Action

	mu          sync.Mutex
	last        *panelStatus
	reachable   bool
	svc         string
	tone        string
	setupCancel context.CancelFunc
	setupToken  string
	lastStart   time.Time
	atLogon     bool // aberto pelo Windows no logon (tray) → avisa que iniciou
	greeted     bool
	deviceDown  time.Time
	notifiedOff bool
	notifiedSvc bool
	needShow    bool // SetVisible falhou (barra de tarefas ainda subindo)
	needIcon    bool // SetIcon falhou: reaplica no próximo ciclo
	stoppedAt   time.Time
}

// newNotifyIconRetry cria o ícone da bandeja. No logon a barra de tarefas pode
// ainda não estar pronta e o Shell_NotifyIcon falha: em vez de morrer (e levar o
// watchdog junto), tenta de novo por até ~3 minutos.
func newNotifyIconRetry(mw *walk.MainWindow, atLogon bool) (*walk.NotifyIcon, error) {
	deadline := time.Now().Add(10 * time.Minute) // logon lento (HDD, muitos apps)
	wait := 2 * time.Second
	for {
		ni, err := walk.NewNotifyIcon(mw)
		if err == nil {
			return ni, nil
		}
		// Uma tentativa pode ter registrado o ícone e falhado depois (NIM_ADD com
		// timeout, ou NIM_SETVERSION): remove o registro parcial, senão toda nova
		// tentativa com o mesmo (janela, id) falharia como duplicata.
		nid := win.NOTIFYICONDATA{HWnd: mw.Handle()}
		nid.CbSize = uint32(unsafe.Sizeof(nid) - unsafe.Sizeof(win.HICON(0)))
		win.Shell_NotifyIcon(win.NIM_DELETE, &nid)
		if !atLogon || time.Now().After(deadline) {
			return nil, err
		}
		LogStartupError(fmt.Errorf("bandeja ainda indisponível (tentando de novo): %w", err))
		time.Sleep(wait)
		if wait < 10*time.Second {
			wait += 2 * time.Second
		}
	}
}

func (t *tray) run(minimized bool) error {
	mw, err := walk.NewMainWindow()
	if err != nil {
		return err
	}
	t.mw = mw
	mw.SetTitle(hostTitle) // diferente do painel, para FindWindow não achar a si mesmo

	if err := t.loadIcons(); err != nil {
		// Sem os .ico (pasta do usuário inacessível etc.): segue com o ícone do
		// próprio exe em vez de não abrir — o watchdog importa mais que a cor.
		LogStartupError(fmt.Errorf("ícones da bandeja: %w", err))
		fallback := walk.IconApplication()
		if ic, ierr := walk.NewIconFromResourceId(7); ierr == nil {
			fallback = ic
		}
		for _, tone := range []string{toneIdle, toneOK, toneWarn, toneBad, toneBusy} {
			t.icons[tone] = fallback
		}
	}
	if ic := t.icons[toneIdle]; ic != nil {
		_ = mw.SetIcon(ic)
	}

	ni, err := newNotifyIconRetry(mw, minimized)
	if err != nil {
		return err
	}
	defer ni.Dispose()
	t.ni = ni
	_ = ni.SetIcon(t.icons[toneIdle])
	_ = ni.SetToolTip(title + " — verificando…")
	t.buildMenu()
	if err := ni.SetVisible(true); err != nil {
		t.needShow = true // tenta de novo a cada ciclo de status
	}
	ni.MouseUp().Attach(func(x, y int, b walk.MouseButton) {
		if b == walk.LeftButton {
			t.openPanel("")
		}
	})
	ni.MessageClicked().Attach(func() { t.openPanel("") })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go t.loop(ctx)
	go func() {
		// O Windows 11 esconde ícones novos no "^". Mostra o do PontoPar na barra
		// uma vez (depois disso respeitamos se o usuário esconder).
		time.Sleep(12 * time.Second)
		promoteTrayIcon()
	}()

	if !minimized {
		// Espera o primeiro status (até ~2s) para abrir já no painel certo.
		go func() {
			deadline := time.Now().Add(2500 * time.Millisecond)
			for time.Now().Before(deadline) {
				t.mu.Lock()
				ready := t.reachable || t.setupCancel != nil
				t.mu.Unlock()
				if ready {
					break
				}
				time.Sleep(150 * time.Millisecond)
			}
			mw.Synchronize(func() { t.openPanel("") })
		}()
	}

	mw.Run()
	cancel()
	t.stopSetupServer()
	return nil
}

// ---------- ícones e menu ----------

func (t *tray) loadIcons() error {
	t.icons = map[string]*walk.Icon{}
	base, err := os.UserCacheDir()
	if err != nil || base == "" {
		base = os.TempDir()
	}
	dir := filepath.Join(base, "PontoParColetor", "icons")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	for _, tone := range []string{toneIdle, toneOK, toneWarn, toneBad, toneBusy} {
		data, err := buildStatusICO(tone)
		if err != nil {
			return err
		}
		path := filepath.Join(dir, fmt.Sprintf("tray-%s-%s.ico", tone, sanitize(t.version)))
		if err := os.WriteFile(path, data, 0o644); err != nil {
			return err
		}
		ic, err := walk.NewIconFromFile(path)
		if err != nil {
			return err
		}
		t.icons[tone] = ic
	}
	return nil
}

func sanitize(s string) string {
	return strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '.' || r == '-' {
			return r
		}
		return '_'
	}, s)
}

func (t *tray) buildMenu() {
	acts := t.ni.ContextMenu().Actions()
	add := func(text string, fn func()) *walk.Action {
		a := walk.NewAction()
		_ = a.SetText(text)
		if fn != nil {
			a.Triggered().Attach(fn)
		}
		_ = acts.Add(a)
		return a
	}

	t.statusAct = add("Verificando o coletor…", nil)
	_ = t.statusAct.SetEnabled(false)
	_ = acts.Add(walk.NewSeparatorAction())

	t.openAct = add("Abrir painel", func() { t.openPanel("") })
	_ = t.openAct.SetDefault(true)
	t.collect = add("Coletar agora", t.collectNow)

	refill, _ := walk.NewMenu()
	sub := func(text string, fn func()) {
		a := walk.NewAction()
		_ = a.SetText(text)
		a.Triggered().Attach(fn)
		_ = refill.Actions().Add(a)
	}
	sub("Tudo desde o início…", func() {
		t.refill("all", 0, "Reenviar ao Thera TODAS as batidas que o relógio ainda guarda?\n\nO Thera ignora as que já tem — nada é duplicado. Pode levar alguns minutos; a coleta normal volta sozinha no fim.")
	})
	sub("Só de hoje", func() { t.refill("days", 1, "") })
	sub("Últimos 7 dias", func() { t.refill("days", 7, "") })
	sub("Últimos 30 dias", func() { t.refill("days", 30, "") })
	_ = refill.Actions().Add(walk.NewSeparatorAction())
	sub("Escolher período…", func() { t.openPanel("#refill") })
	t.refillMn = walk.NewMenuAction(refill)
	_ = t.refillMn.SetText("Refill (reenviar batidas)")
	_ = acts.Add(t.refillMn)

	_ = acts.Add(walk.NewSeparatorAction())
	t.startAct = add("Iniciar coletor", t.startService)
	_ = t.startAct.SetVisible(false)
	t.autoAct = add("Iniciar com o Windows", t.toggleAutostart)
	_ = t.autoAct.SetCheckable(true)
	add("Abrir pasta de dados", func() {
		dir, _ := setup.ActiveConfigDir()
		shellOpen(dir)
	})
	_ = acts.Add(walk.NewSeparatorAction())
	add("Sair da bandeja", func() {
		if askYesNo("Sair da bandeja?", "O ícone some, mas a coleta CONTINUA rodando em segundo plano.\nEle volta sozinho na próxima vez que você entrar no Windows.") {
			t.mw.Close()
		}
	})
}

// ---------- loop de status ----------

type panelStatus struct {
	Mode           string `json:"mode"`
	Version        string `json:"version"`
	Service        string `json:"service"`
	Autostart      bool   `json:"autostart"`
	AutostartKnown bool   `json:"autostartKnown"`
	ConfigValid    bool   `json:"configValid"`
	DeviceIP       string `json:"deviceIp"`
	State          struct {
		DeviceOnline      bool          `json:"deviceOnline"`
		DeviceLastErr     string        `json:"deviceLastError"`
		TheraOnline       bool          `json:"theraOnline"`
		TheraLastErr      string        `json:"theraLastError"`
		LastTickAt        time.Time     `json:"lastTickAt"`
		LastSyncAt        time.Time     `json:"lastSyncAt"`
		ForwardedToday    int64         `json:"forwardedToday"`
		FullSyncRunning   bool          `json:"fullSyncRunning"`
		FullSyncForwarded int64         `json:"fullSyncForwarded"`
		FullSyncReadTotal int64         `json:"fullSyncReadTotal"`
		FullSyncSource    string        `json:"fullSyncSource"`
		FullSyncLast      *audit.Record `json:"fullSyncLast"`
		FullSyncLastAt    time.Time     `json:"fullSyncLastAt"`
	} `json:"state"`
}

func (t *tray) loop(ctx context.Context) {
	for {
		t.tick()
		select {
		case <-ctx.Done():
			return
		case <-time.After(pollInterval):
		}
	}
}

func (t *tray) tick() {
	port := loadPort()
	st, err := fetchStatus(port)
	svc, _ := winservice.Status()

	t.mu.Lock()
	prev := t.last
	t.svc = svc
	t.reachable = err == nil
	if err == nil {
		t.last = st
	}
	inSetup := t.setupCancel != nil
	t.mu.Unlock()

	switch {
	case err == nil && st.Mode == webui.ModeService:
		// Painel do serviço respondendo. Se o app tinha subido o modo setup (que
		// já foi derrubado para liberar a porta), migra para a cópia instalada.
		if t.relaunchIfOutdated(st) {
			return
		}
	case svc == "nao instalado":
		if !inSetup {
			t.startSetupServer(port)
		}
	case svc == "rodando" || svc == "iniciando":
		if inSetup {
			// O serviço acabou de ser instalado: libera a porta para o painel dele.
			t.stopSetupServer()
		}
	case svc == "parado":
		if inSetup {
			t.stopSetupServer()
		}
		// Watchdog: com "Iniciar com o Windows" ligado o coletor TEM de estar
		// rodando. Se está parado (falhou no boot, alguém parou, o recovery
		// desistiu), religa — sem pedir nada, a instalação deu esse direito ao
		// usuário. Com o início automático desligado, respeita a parada.
		auto, aerr := winservice.IsAutoStart()
		t.mu.Lock()
		if t.stoppedAt.IsZero() {
			t.stoppedAt = time.Now()
		}
		due := time.Since(t.lastStart) >= restartEvery && time.Since(t.stoppedAt) >= stoppedGrace
		if aerr == nil && auto && due {
			t.lastStart = time.Now()
		}
		t.mu.Unlock()
		if aerr == nil && auto && due {
			// Só avisa se foi ESTA bandeja que religou (ErrAlreadyRunning = o
			// recovery ou outra sessão chegou antes).
			if winservice.Start() == nil {
				t.notify(toneOK, "Coletor religado", "O coletor estava parado e foi religado automaticamente.")
			}
		}
	}
	if svc != "parado" {
		t.mu.Lock()
		t.stoppedAt = time.Time{}
		t.mu.Unlock()
	}

	t.mw.Synchronize(func() { t.render(prev) })
}

// render atualiza ícone, tooltip e menu, e dispara notificações de transição.
func (t *tray) render(prev *panelStatus) {
	t.mu.Lock()
	st, reachable, svc, inSetup := t.last, t.reachable, t.svc, t.setupCancel != nil
	t.mu.Unlock()

	tone, line := toneIdle, "Verificando o coletor…"
	running := svc == "rodando" || svc == "iniciando"
	switch {
	case inSetup || svc == "nao instalado":
		tone, line = toneWarn, "Coletor não instalado — clique para configurar"
	case !reachable && svc == "parado":
		tone, line = toneBad, "Coletor PARADO — as batidas não estão indo ao Thera"
	case !reachable && running:
		tone, line = toneIdle, "Coletor iniciando…"
	case !reachable:
		tone, line = toneBad, "Sem resposta do coletor ("+orDash(svc)+")"
	case !st.ConfigValid:
		tone, line = toneWarn, "Configuração incompleta — abra o painel"
	case st.State.FullSyncRunning:
		tone, line = toneBusy, fmt.Sprintf("Refill em andamento — %s enviadas", thousands(st.State.FullSyncForwarded))
	case st.State.LastTickAt.IsZero():
		tone, line = toneIdle, "Conectando ao relógio…"
	case !st.State.DeviceOnline:
		tone, line = toneBad, "Sem conexão com o relógio ("+st.DeviceIP+")"
	case !st.State.TheraOnline && st.State.TheraLastErr != "":
		tone, line = toneWarn, "Sem conexão com o Thera — batidas guardadas no relógio"
	default:
		tone, line = toneOK, fmt.Sprintf("Coletando · %s batidas hoje", thousands(st.State.ForwardedToday))
	}

	if t.tone != tone || t.needIcon {
		// Só considera aplicado se o shell aceitou (no logon ele pode recusar).
		if err := t.ni.SetIcon(t.icons[tone]); err == nil {
			t.tone, t.needIcon = tone, false
		} else {
			t.needIcon = true
		}
		_ = t.mw.SetIcon(t.icons[tone])
	}
	if t.needShow {
		if t.ni.SetVisible(true) == nil {
			t.needShow = false
		}
	}
	tip := title + "\n" + line
	if len(tip) > 120 { // limite do tooltip da bandeja
		tip = tip[:120]
	}
	_ = t.ni.SetToolTip(tip)
	_ = t.statusAct.SetText(line)
	_ = t.statusAct.SetImage(t.icons[tone])

	canAct := reachable && st != nil && st.Mode == webui.ModeService && st.ConfigValid
	busy := canAct && st.State.FullSyncRunning
	_ = t.collect.SetEnabled(canAct && !busy)
	_ = t.refillMn.SetEnabled(canAct && !busy)
	_ = t.startAct.SetVisible(!reachable && svc != "nao instalado" && !running)
	_ = t.autoAct.SetEnabled(reachable && st != nil && st.AutostartKnown)
	if st != nil {
		_ = t.autoAct.SetChecked(st.Autostart)
	}

	// Aberto pelo Windows no logon: confirma (uma vez) que o coletor subiu junto.
	if t.atLogon && !t.greeted && tone != toneIdle {
		t.greeted = true
		t.notify(tone, "PontoPar Coletor iniciado com o Windows", line)
	}

	// --- notificações de transição ---
	if reachable && prev != nil && st != nil {
		// Refill terminou.
		if prev.State.FullSyncRunning && !st.State.FullSyncRunning {
			if r := st.State.FullSyncLast; r != nil {
				if r.Err != "" {
					t.notify(toneBad, "Refill interrompido", r.Err+"\nAbra o painel para retomar.")
				} else {
					msg := fmt.Sprintf("%s batidas reenviadas ao Thera (de %s lidas).", thousands(int64(r.Forwarded)), thousands(int64(r.Read)))
					if r.RotatedGap > 0 {
						msg += fmt.Sprintf("\nAtenção: o relógio já tinha apagado %s registros antigos.", thousands(r.RotatedGap))
					}
					if r.NoRegistration > 0 {
						msg += fmt.Sprintf("\n%s batidas sem matrícula no relógio.", thousands(int64(r.NoRegistration)))
					}
					t.notify(toneOK, "Refill concluído", msg)
				}
			}
		}
	}
	// Relógio fora por mais de offlineGrace.
	if reachable && st != nil && st.Mode == webui.ModeService && !st.State.LastTickAt.IsZero() {
		if !st.State.DeviceOnline {
			if t.deviceDown.IsZero() {
				t.deviceDown = time.Now()
			}
			if !t.notifiedOff && time.Since(t.deviceDown) > offlineGrace {
				t.notifiedOff = true
				t.notify(toneBad, "Sem conexão com o relógio", "O coletor não consegue falar com o Control iD em "+st.DeviceIP+". Confira se ele está ligado e na rede. As batidas ficam guardadas nele.")
			}
		} else {
			if t.notifiedOff {
				t.notify(toneOK, "Relógio reconectado", "A coleta voltou ao normal. O que ficou guardado no relógio já está sendo enviado.")
			}
			t.deviceDown, t.notifiedOff = time.Time{}, false
		}
	}
	// Coletor parado (sem permissão para religar sozinho).
	if !reachable && svc == "parado" {
		if !t.notifiedSvc && time.Since(t.started) > 8*time.Second {
			t.notifiedSvc = true
			t.notify(toneBad, "Coletor parado", "As batidas não estão indo para o Thera. Clique com o botão direito no ícone do PontoPar → “Iniciar coletor”.")
		}
	} else if reachable {
		t.notifiedSvc = false
	}
}

func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

func (t *tray) notify(tone, head, msg string) {
	switch tone {
	case toneBad:
		_ = t.ni.ShowError(head, msg)
	case toneWarn:
		_ = t.ni.ShowWarning(head, msg)
	default:
		_ = t.ni.ShowInfo(head, msg)
	}
}

// relaunchIfOutdated: depois de um auto-update o serviço já roda a versão nova,
// mas este app (aberto no logon) ainda é o antigo. Se o .exe no disco é mais novo
// que este processo, relança a si mesmo. A checagem de data evita loop quando o
// app roda de outra pasta (cópia antiga no Downloads, por exemplo).
func (t *tray) relaunchIfOutdated(st *panelStatus) bool {
	if st.Version == "" || st.Version == t.version {
		return false
	}
	exe, err := os.Executable()
	if err != nil {
		return false
	}
	fi, err := os.Stat(exe)
	if err != nil || !fi.ModTime().After(t.started) {
		return false
	}
	t.relaunch(exe)
	return true
}

func (t *tray) relaunch(exe string) {
	windows.CloseHandle(t.mutex) // libera a instância única para o novo processo
	cmd := exec.Command(exe, "tray")
	cmd.Dir = filepath.Dir(exe)
	if cmd.Start() == nil {
		t.mw.Synchronize(func() { t.mw.Close() })
	}
}

// ---------- modo setup ----------

func (t *tray) startSetupServer(port int) {
	dir, err := setup.ActiveConfigDir()
	if err != nil {
		return
	}
	ctrl := &setupCtrl{dir: dir, version: t.version, install: func() error {
		return runElevated(selfExe(), "setup-install", dir)
	}}
	srv := webui.NewServer(ctrl, fmt.Sprintf("127.0.0.1:%d", port), nil)
	ln, err := srv.Listen()
	if err != nil {
		return // porta ocupada por outra coisa; tenta no próximo tick
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.mu.Lock()
	t.setupCancel = cancel
	t.setupToken = srv.Token()
	t.mu.Unlock()
	go func() { _ = srv.ServeListener(ctx, ln) }()
}

func (t *tray) stopSetupServer() {
	t.mu.Lock()
	cancel := t.setupCancel
	t.setupCancel = nil
	t.setupToken = ""
	t.mu.Unlock()
	if cancel != nil {
		cancel()
		time.Sleep(300 * time.Millisecond) // dá tempo do socket fechar antes do serviço tentar o bind
	}
}

// ---------- ações ----------

func (t *tray) openPanel(hash string) {
	t.mu.Lock()
	reachable, inSetup, svc := t.reachable, t.setupCancel != nil, t.svc
	t.mu.Unlock()
	if !reachable && !inSetup {
		if svc == "parado" {
			if askYesNo("Coletor parado", "O coletor está parado — as batidas não estão indo para o Thera.\n\nIniciar agora?") {
				t.startService()
			}
			return
		}
		if svc == "rodando" || svc == "iniciando" {
			_ = t.ni.ShowInfo("Coletor iniciando", "O painel abre em alguns segundos. Tente de novo já já.")
			return
		}
	}
	openPanel(panelURL(loadPort()), hash)
}

func (t *tray) collectNow() {
	if err := postPanel(loadPort(), "/api/collect-now", nil); err != nil {
		t.notify(toneBad, "Não consegui coletar agora", err.Error())
		return
	}
	t.notify(toneOK, "Coletando agora", "As batidas novas chegam ao Thera em segundos.")
}

func (t *tray) refill(mode string, days int, confirm string) {
	if confirm == "" {
		label := "de hoje"
		if days > 1 {
			label = fmt.Sprintf("dos últimos %d dias", days)
		}
		confirm = "Reenviar ao Thera as batidas " + label + " que estão no relógio?\n\nO Thera ignora as que já tem — nada é duplicado."
	}
	if !askYesNo("Refill", confirm) {
		return
	}
	body := map[string]any{"mode": mode, "days": days, "source": "bandeja"}
	if err := postPanel(loadPort(), "/api/refill", body); err != nil {
		t.notify(toneBad, "Não consegui iniciar o refill", err.Error())
		return
	}
	t.notify(toneBusy, "Refill iniciado", "Avisamos aqui quando terminar. O progresso está no painel.")
	go func() { time.Sleep(time.Second); t.tick() }()
}

func (t *tray) toggleAutostart() {
	t.mu.Lock()
	st := t.last
	t.mu.Unlock()
	if st == nil {
		return
	}
	want := !st.Autostart
	if !want && !askYesNo("Desligar o início automático?", "Depois de reiniciar o Windows o coletor NÃO vai subir sozinho, e as batidas não chegam ao Thera até alguém iniciar à mão.\n\nDesligar mesmo assim?") {
		_ = t.autoAct.SetChecked(true)
		return
	}
	if err := postPanel(loadPort(), "/api/autostart", map[string]any{"enabled": want}); err != nil {
		t.notify(toneBad, "Não consegui alterar", err.Error())
		_ = t.autoAct.SetChecked(!want)
		return
	}
	go t.tick()
}

func (t *tray) startService() {
	if err := winservice.Start(); err == nil || errors.Is(err, winservice.ErrAlreadyRunning) {
		t.notify(toneOK, "Coletor iniciado", "A coleta voltou. As batidas guardadas no relógio já estão sendo enviadas.")
		go func() { time.Sleep(2 * time.Second); t.tick() }()
		return
	}
	// Sem permissão (instalação antiga) ou serviço quebrado: repara com UAC. O
	// "install-start" é idempotente — corrige caminho, auto-start, recovery,
	// permissões e o ícone no logon, e inicia.
	exe := setup.InstalledExePath()
	if _, err := os.Stat(exe); err != nil {
		exe = selfExe()
	}
	if err := runElevated(exe, "install-start", filepath.Dir(exe)); err != nil {
		t.notify(toneBad, "Não consegui iniciar o coletor", err.Error())
	}
}

// ---------- painel: HTTP ----------

// loadPort descobre a porta do painel: a REAL, publicada pelo serviço em
// panel.json; sem ela, a do config (ou a padrão).
func loadPort() int {
	dir, err := setup.ActiveConfigDir()
	if err == nil {
		if pi := app.ReadPanelInfo(dir); pi.Addr != "" {
			if _, p, err := net.SplitHostPort(pi.Addr); err == nil {
				if n, err := strconv.Atoi(p); err == nil && n > 0 {
					return n
				}
			}
		}
		if cfg, err := config.Load(dir); err == nil {
			return cfg.UIPort()
		}
	}
	return config.Default().UIPort()
}

func panelURL(port int) string { return fmt.Sprintf("http://127.0.0.1:%d/", port) }

var httpc = &http.Client{Timeout: 4 * time.Second}

func fetchStatus(port int) (*panelStatus, error) {
	resp, err := httpc.Get(panelURL(port) + "api/status")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("painel respondeu %d", resp.StatusCode)
	}
	var st panelStatus
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
		return nil, err
	}
	return &st, nil
}

// postPanel faz uma mutação no painel do serviço, autenticada pelo token que o
// serviço publica em panel.json (tenta de novo uma vez se o serviço reiniciou).
func postPanel(port int, path string, body any) error {
	var payload []byte
	if body != nil {
		payload, _ = json.Marshal(body)
	}
	for attempt := 0; attempt < 2; attempt++ {
		req, _ := http.NewRequest(http.MethodPost, strings.TrimSuffix(panelURL(port), "/")+path, bytes.NewReader(payload))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set(webui.TokenHeader, readPanelToken())
		resp, err := httpc.Do(req)
		if err != nil {
			return fmt.Errorf("o coletor não respondeu: %w", err)
		}
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		if resp.StatusCode == http.StatusForbidden && attempt == 0 {
			time.Sleep(500 * time.Millisecond)
			continue
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			msg := strings.TrimSpace(string(data))
			if msg == "" {
				msg = resp.Status
			}
			return fmt.Errorf("%s", msg)
		}
		return nil
	}
	return fmt.Errorf("o painel recusou o pedido (token)")
}

func readPanelToken() string {
	dir, err := setup.ActiveConfigDir()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(app.ReadPanelInfo(dir).Token)
}

// ---------- janela do painel ----------

// openPanel traz a janela do painel para a frente se já estiver aberta; senão
// abre o Edge (ou Chrome) em modo app — uma janela própria, sem abas nem barra
// de endereço, com o ícone do PontoPar. Sem nenhum dos dois, usa o navegador
// padrão.
func openPanel(url, hash string) {
	if hash == "" {
		if hwnd := win.FindWindow(nil, syscall.StringToUTF16Ptr(panelTitle)); hwnd != 0 {
			if win.IsIconic(hwnd) {
				win.ShowWindow(hwnd, win.SW_RESTORE)
			}
			win.SetForegroundWindow(hwnd)
			return
		}
	}
	target := url + hash
	for _, exe := range browserCandidates() {
		cmd := exec.Command(exe, "--app="+target, "--window-size=1280,860", "--no-first-run", "--no-default-browser-check")
		if cmd.Start() == nil {
			return
		}
	}
	shellOpen(target)
}

func browserCandidates() []string {
	var out []string
	seen := map[string]bool{}
	addPath := func(p string) {
		p = strings.Trim(strings.TrimSpace(p), `"`)
		if p == "" || seen[strings.ToLower(p)] {
			return
		}
		if _, err := os.Stat(p); err == nil {
			seen[strings.ToLower(p)] = true
			out = append(out, p)
		}
	}
	for _, name := range []string{"msedge.exe", "chrome.exe"} {
		for _, root := range []registry.Key{registry.CURRENT_USER, registry.LOCAL_MACHINE} {
			k, err := registry.OpenKey(root, `SOFTWARE\Microsoft\Windows\CurrentVersion\App Paths\`+name, registry.QUERY_VALUE)
			if err == nil {
				v, _, _ := k.GetStringValue("")
				k.Close()
				addPath(v)
			}
		}
	}
	for _, env := range []string{"ProgramFiles(x86)", "ProgramFiles", "LocalAppData"} {
		base := os.Getenv(env)
		if base == "" {
			continue
		}
		addPath(filepath.Join(base, `Microsoft\Edge\Application\msedge.exe`))
		addPath(filepath.Join(base, `Google\Chrome\Application\chrome.exe`))
	}
	return out
}

// promoteTrayIcon deixa o ícone do PontoPar visível na barra do Windows 11
// (HKCU\Control Panel\NotifyIconSettings\<id>\IsPromoted=1). Faz isso UMA vez
// por usuário (marca em %LocalAppData%): se depois a pessoa esconder o ícone, a
// escolha dela vale. No Windows 10 a chave não existe e nada acontece.
func promoteTrayIcon() {
	base, err := os.UserCacheDir()
	if err != nil {
		return
	}
	flag := filepath.Join(base, "PontoParColetor", "tray-promoted")
	if _, err := os.Stat(flag); err == nil {
		return
	}
	root, err := registry.OpenKey(registry.CURRENT_USER, `Control Panel\NotifyIconSettings`, registry.ENUMERATE_SUB_KEYS)
	if err != nil {
		return
	}
	defer root.Close()
	names, err := root.ReadSubKeyNames(-1)
	if err != nil {
		return
	}
	exeName := strings.ToLower(filepath.Base(selfExe()))
	done := false
	for _, n := range names {
		k, err := registry.OpenKey(registry.CURRENT_USER, `Control Panel\NotifyIconSettings\`+n, registry.QUERY_VALUE|registry.SET_VALUE)
		if err != nil {
			continue
		}
		path, _, err := k.GetStringValue("ExecutablePath")
		if err == nil && strings.HasSuffix(strings.ToLower(path), `\`+exeName) {
			if k.SetDWordValue("IsPromoted", 1) == nil {
				done = true
			}
		}
		k.Close()
	}
	if done {
		_ = os.MkdirAll(filepath.Dir(flag), 0o755)
		_ = os.WriteFile(flag, []byte(time.Now().Format(time.RFC3339)), 0o644)
	}
}

// ---------- utilidades Win32 ----------

func singleInstance() (windows.Handle, bool) {
	name, _ := windows.UTF16PtrFromString(mutexName)
	h, err := windows.CreateMutex(nil, false, name)
	if err == windows.ERROR_ALREADY_EXISTS {
		if h != 0 {
			windows.CloseHandle(h)
		}
		return 0, true
	}
	return h, false
}

func selfExe() string {
	exe, err := os.Executable()
	if err != nil {
		return "pontopar-coletor.exe"
	}
	return exe
}

func runElevated(exe, args, dir string) error {
	if !win.ShellExecute(0, syscall.StringToUTF16Ptr("runas"), syscall.StringToUTF16Ptr(exe), syscall.StringToUTF16Ptr(args), syscall.StringToUTF16Ptr(dir), win.SW_SHOWNORMAL) {
		return fmt.Errorf("o Windows não abriu o pedido de administrador (cancelado?)")
	}
	return nil
}

func shellOpen(target string) {
	win.ShellExecute(0, syscall.StringToUTF16Ptr("open"), syscall.StringToUTF16Ptr(target), nil, nil, win.SW_SHOWNORMAL)
}

func msgBox(head, msg string, flags uint32) int32 {
	return win.MessageBox(0, syscall.StringToUTF16Ptr(msg), syscall.StringToUTF16Ptr(head), flags|win.MB_SETFOREGROUND|win.MB_TOPMOST)
}

func askYesNo(head, msg string) bool {
	return msgBox(head, msg, win.MB_YESNO|win.MB_ICONQUESTION) == win.IDYES
}

// thousands formata 12345 como "12.345".
func thousands(n int64) string {
	s := fmt.Sprintf("%d", n)
	neg := strings.HasPrefix(s, "-")
	if neg {
		s = s[1:]
	}
	var b strings.Builder
	for i, r := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte('.')
		}
		b.WriteRune(r)
	}
	if neg {
		return "-" + b.String()
	}
	return b.String()
}
