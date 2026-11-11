//go:build windows

// Package winservice integra o coletor com o Service Control Manager (SCM) do
// Windows: registra o handler svc.Handler, instala/desinstala o serviço com
// StartType=Automatic e RecoveryAction=Restart, e implementa start/stop/status.
//
// Todo o código específico do Windows fica isolado aqui por build tag; o
// service_other.go fornece stubs para os demais SOs (permite `go vet` e build
// cruzado do resto do projeto em qualquer host).
package winservice

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/p-lacerda/pontopar-coletor/internal/autorun"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/debug"
	"golang.org/x/sys/windows/svc/eventlog"
	"golang.org/x/sys/windows/svc/mgr"
)

// ServiceName é o nome interno do serviço no SCM.
const ServiceName = "PontoParColetor"

// serviceDisplay é o nome exibido no snap-in de Serviços.
const serviceDisplay = "Coletor PontoPar"

// serviceDesc é a descrição do serviço.
const serviceDesc = "Coleta batidas do Control iD iDFace e encaminha ao Thera"

// stopWaitHintMs é o WaitHint enviado ao SCM durante o Stop, pedindo tempo
// extra enquanto o coletor finaliza (flush) — evita o SCM matar o processo aos
// ~30s antes do shutdown limpo.
const stopWaitHintMs = 20000

// exitUnexpected é o código de saída quando o coletor para sem o Windows pedir:
// qualquer valor != 0 aciona o recovery (reinício automático).
const exitUnexpected = 1067 // ERROR_PROCESS_ABORTED

// elog é o log de eventos do serviço (Visualizador de Eventos), setado em Run.
var elog debug.Log

// RunFunc é a função que o serviço executa; deve rodar até ctx ser cancelado.
// Normalmente é collector.Run.
type RunFunc func(ctx context.Context)

// coletorService implementa svc.Handler.
type coletorService struct {
	run RunFunc
}

// Execute é o handler do SCM. Segue o protocolo oficial: StartPending ->
// Running(Accepts) -> ... -> StopPending -> return. Não aceita Pause/Continue
// (ruído para um coletor); só Stop+Shutdown.
func (s *coletorService) Execute(args []string, r <-chan svc.ChangeRequest, changes chan<- svc.Status) (ssec bool, errno uint32) {
	const cmdsAccepted = svc.AcceptStop | svc.AcceptShutdown

	changes <- svc.Status{State: svc.StartPending}

	// Contexto que cancela o worker no Stop/Shutdown.
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		s.run(ctx)
		close(done)
	}()

	changes <- svc.Status{State: svc.Running, Accepts: cmdsAccepted}

loop:
	for {
		select {
		case c := <-r:
			switch c.Cmd {
			case svc.Interrogate:
				changes <- c.CurrentStatus
			case svc.Stop, svc.Shutdown:
				break loop
			default:
				elog.Error(1, fmt.Sprintf("controle inesperado #%d", c.Cmd))
			}
		case <-done:
			// O trabalho terminou SEM o Windows pedir (ex.: o supervisor saiu).
			// Antes o serviço ficava "Em execução" sem coletar nada. Agora sai com
			// código de erro: como o recovery vale também para falhas sem crash
			// (SetRecoveryActionsOnNonCrashFailures), o SCM religa o coletor.
			elog.Error(1, "o coletor parou sozinho; saindo com erro para o Windows reiniciar o serviço")
			cancel()
			return false, exitUnexpected
		}
	}

	// Sinaliza StopPending com WaitHint e cancela o worker; dá chance de flush.
	changes <- svc.Status{State: svc.StopPending, WaitHint: stopWaitHintMs}
	cancel()

	// Aguarda o worker terminar, renovando o WaitHint periodicamente para o SCM
	// não matar o processo antes do flush terminar.
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-done:
			return false, 0
		case <-ticker.C:
			changes <- svc.Status{State: svc.StopPending, WaitHint: stopWaitHintMs}
		}
	}
}

// IsWindowsService informa se estamos rodando sob o SCM.
func IsWindowsService() (bool, error) {
	return svc.IsWindowsService()
}

// RunAsService roda o coletor sob o SCM (produção) ou em foreground via
// debug.Run (isDebug=true, para dev). run é a função de trabalho.
func RunAsService(run RunFunc, isDebug bool) error {
	var err error
	if isDebug {
		elog = debug.New(ServiceName)
	} else {
		elog, err = eventlog.Open(ServiceName)
		if err != nil {
			return err
		}
	}
	defer elog.Close()

	elog.Info(1, fmt.Sprintf("%s iniciando", ServiceName))
	runner := svc.Run
	if isDebug {
		runner = debug.Run
	}
	err = runner(ServiceName, &coletorService{run: run})
	if err != nil {
		elog.Error(1, fmt.Sprintf("%s falhou: %v", ServiceName, err))
		return err
	}
	elog.Info(1, fmt.Sprintf("%s parado", ServiceName))
	return nil
}

// Install registra o serviço no SCM com auto-start e recovery=restart, e
// registra a fonte de eventos. Requer processo ELEVADO (Admin). exePath é o
// caminho do executável a registrar (normalmente os.Executable()).
func Install(exePath string) error {
	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("nao foi possivel conectar ao SCM (rode como Administrador): %w", err)
	}
	defer m.Disconnect()

	if s, err := m.OpenService(ServiceName); err == nil {
		s.Close()
		return fmt.Errorf("servico %s ja existe", ServiceName)
	}

	s, err := m.CreateService(ServiceName, exePath, mgr.Config{
		DisplayName: serviceDisplay,
		Description: serviceDesc,
		StartType:   mgr.StartAutomatic, // sobe no boot, sem ninguém logado
	})
	if err != nil {
		return fmt.Errorf("criar servico: %w", err)
	}
	defer s.Close()

	if err := harden(s); err != nil {
		_ = s.Delete()
		return err
	}

	// Fonte do event log (Visualizador de Eventos). É só diagnóstico: uma fonte
	// que sobrou de uma instalação anterior ("already exists") NÃO pode derrubar
	// a instalação — antes isso apagava o serviço recém-criado em silêncio e o
	// coletor nunca mais subia com o Windows.
	if err = eventlog.InstallAsEventCreate(ServiceName, eventlog.Error|eventlog.Warning|eventlog.Info); err != nil &&
		!strings.Contains(err.Error(), "already exists") {
		// Best-effort: segue sem a fonte de eventos (o log em arquivo continua).
		_ = err
	}
	return nil
}

// interactiveUsersSDDL é o descritor de segurança do serviço: o padrão do
// Windows (SYSTEM e Administradores com controle total; usuários interativos
// podem consultar) MAIS o direito de INICIAR (RP) para usuários interativos.
// Assim o app da bandeja consegue religar o coletor sem pedir administrador.
// Parar/reconfigurar continua restrito a Administradores e ao próprio serviço.
const interactiveUsersSDDL = "D:(A;;CCLCSWRPWPDTLOCRRC;;;SY)(A;;CCDCLCSWRPWPDTLOCRSDRCWDWO;;;BA)(A;;CCLCSWRPLOCRRC;;;IU)(A;;CCLCSWLOCRRC;;;SU)"

// harden aplica o que torna o serviço confiável: recovery que reinicia SEMPRE
// (também quando o processo sai com erro sem "crashar"), e a permissão de
// iniciar para usuários interativos.
func harden(s *mgr.Service) error {
	// Recovery: reinicia em 5s, 15s e depois a cada 60s (a 3ª ação vale para
	// todas as falhas seguintes); zera o contador após 1 dia. É também o
	// mecanismo que relança o serviço após um auto-update (saída != 0).
	if err := s.SetRecoveryActions([]mgr.RecoveryAction{
		{Type: mgr.ServiceRestart, Delay: 5 * time.Second},
		{Type: mgr.ServiceRestart, Delay: 15 * time.Second},
		{Type: mgr.ServiceRestart, Delay: 60 * time.Second},
	}, uint32((24 * time.Hour).Seconds())); err != nil {
		return fmt.Errorf("configurar recovery: %w", err)
	}
	// Sem isto, um "parou com erro" reportado pelo próprio serviço (ex.: falha
	// no arranque) NÃO dispara o recovery — e o coletor ficava parado até
	// alguém notar.
	if err := s.SetRecoveryActionsOnNonCrashFailures(true); err != nil {
		return fmt.Errorf("configurar recovery para falhas sem crash: %w", err)
	}
	if sd, err := windows.SecurityDescriptorFromString(interactiveUsersSDDL); err == nil {
		if dacl, _, derr := sd.DACL(); derr == nil {
			// Best-effort: sem isto, a bandeja ainda consegue pedir elevação.
			_ = windows.SetSecurityInfo(s.Handle, windows.SE_SERVICE, windows.DACL_SECURITY_INFORMATION, nil, nil, dacl, nil)
		}
	}
	return nil
}

// Uninstall remove o serviço e a fonte de eventos. Requer Admin.
func Uninstall() error {
	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("nao foi possivel conectar ao SCM (rode como Administrador): %w", err)
	}
	defer m.Disconnect()

	_ = autorun.Disable() // o app da bandeja deixa de abrir no logon

	s, err := m.OpenService(ServiceName)
	if err != nil {
		return fmt.Errorf("servico %s nao esta instalado", ServiceName)
	}
	defer s.Close()

	// 1) Manual ANTES de parar: qualquer app da bandeja que ainda esteja aberto
	// vê o início automático desligado e não religa o serviço (o watchdog só age
	// com Automatic). 2) Para e espera parar. 3) Só então remove — sem isso o SCM
	// só marcava para exclusão e o coletor seguia rodando até o reboot.
	if cfg, cerr := s.Config(); cerr == nil {
		cfg.StartType = mgr.StartManual
		_ = s.UpdateConfig(cfg)
	}
	if st, qerr := s.Query(); qerr == nil && st.State != svc.Stopped {
		if err := controlService(svc.Stop, svc.Stopped); err != nil {
			return fmt.Errorf("parar o coletor antes de remover: %w", err)
		}
	}

	if err = s.Delete(); err != nil {
		return fmt.Errorf("remover servico: %w", err)
	}
	// Não fatal: o serviço já foi removido; a fonte de eventos é só diagnóstico.
	_ = eventlog.Remove(ServiceName)
	return nil
}

// EnsureHealthy é o AUTORREPARO que o serviço roda a cada arranque (como
// LocalSystem, sem UAC). Existe porque quem recebe uma versão nova pelo
// auto-update NÃO passa pelo instalador: sem isto, essas máquinas ficavam sem o
// app da bandeja no logon, sem a permissão de iniciar para usuários e sem o
// recovery para falhas sem crash. Idempotente e best-effort: devolve o que
// consertou (para o log) e o primeiro erro, mas nunca impede a coleta.
func EnsureHealthy(exePath string) (fixed []string, err error) {
	m, err := mgr.Connect()
	if err != nil {
		return nil, fmt.Errorf("conectar ao SCM: %w", err)
	}
	defer m.Disconnect()
	s, err := m.OpenService(ServiceName)
	if err != nil {
		return nil, fmt.Errorf("abrir servico: %w", err)
	}
	defer s.Close()
	// Os passos são independentes: uma falha no recovery não pode impedir o
	// registro do app no logon (e vice-versa). Reaplicar o harden é barato e
	// idempotente, então não entra na lista de "consertos" do log.
	var errs []error
	if herr := harden(s); herr != nil {
		errs = append(errs, herr)
	}
	cfg, cerr := s.Config()
	if cerr != nil {
		errs = append(errs, fmt.Errorf("ler configuracao do servico: %w", cerr))
	} else if cfg.StartType == mgr.StartAutomatic && !autorun.Registered() {
		// Com "Iniciar com o Windows" ligado, o app da bandeja tem de abrir no
		// logon. Se o valor Run não existe, cria. Se existe mas o usuário o
		// desabilitou no Gerenciador de Tarefas, respeita.
		if aerr := autorun.Enable(exePath); aerr != nil {
			errs = append(errs, aerr)
		} else {
			fixed = append(fixed, "app da bandeja registrado no logon")
		}
	}
	return fixed, errors.Join(errs...)
}

// openLimited abre o serviço pedindo SÓ os direitos necessários. mgr.Connect()
// pede SC_MANAGER_ALL_ACCESS, que um usuário comum não tem — por isso a tela
// antiga mostrava "não foi possível consultar" para quem não era admin. Com
// SC_MANAGER_CONNECT + o direito específico, consultar funciona para qualquer
// usuário interativo e iniciar funciona depois do harden (ACL com RP para IU).
func openLimited(access uint32) (scm, svcH windows.Handle, err error) {
	scm, err = windows.OpenSCManager(nil, nil, windows.SC_MANAGER_CONNECT)
	if err != nil {
		return 0, 0, fmt.Errorf("conectar ao SCM: %w", err)
	}
	name, _ := windows.UTF16PtrFromString(ServiceName)
	svcH, err = windows.OpenService(scm, name, access)
	if err != nil {
		windows.CloseServiceHandle(scm)
		return 0, 0, err
	}
	return scm, svcH, nil
}

// Start inicia o serviço via SCM (funciona sem admin após o harden).
func Start() error {
	scm, h, err := openLimited(windows.SERVICE_START | windows.SERVICE_QUERY_STATUS)
	if err != nil {
		if err == windows.ERROR_SERVICE_DOES_NOT_EXIST {
			return fmt.Errorf("servico %s nao esta instalado", ServiceName)
		}
		return fmt.Errorf("nao foi possivel acessar o servico: %w", err)
	}
	defer windows.CloseServiceHandle(scm)
	defer windows.CloseServiceHandle(h)
	if err := windows.StartService(h, 0, nil); err != nil {
		if err == windows.ERROR_SERVICE_ALREADY_RUNNING {
			return ErrAlreadyRunning
		}
		return fmt.Errorf("nao foi possivel iniciar o servico: %w", err)
	}
	return nil
}

// ErrAlreadyRunning: Start foi chamado com o serviço já rodando (outro processo
// — o recovery do Windows, outra bandeja — chegou antes). Não é falha.
var ErrAlreadyRunning = errors.New("o servico ja esta rodando")

// InstallAndStart deixa o coletor pronto para produção: instala caso ainda não
// exista, REPARA uma instalação existente (caminho do exe, auto-start,
// recovery, permissões), registra o app da bandeja no logon e inicia agora. É
// idempotente: rodar de novo conserta o que estiver errado. É chamado pelo
// instalador e pelo processo elevado que a bandeja abre após o UAC.
func InstallAndStart(exePath string) error {
	if err := Install(exePath); err != nil {
		if !strings.Contains(err.Error(), "ja existe") {
			return err
		}
		if err := updateInstalledPath(exePath); err != nil {
			return err
		}
	}
	if err := autorun.Enable(exePath); err != nil {
		return fmt.Errorf("registrar o app na inicialização do Windows: %w", err)
	}
	st, err := Status()
	if err != nil {
		return err
	}
	if st == "rodando" || st == "iniciando" {
		return nil
	}
	if err := Start(); err != nil && !errors.Is(err, ErrAlreadyRunning) {
		return err
	}
	return waitState(svc.Running, 20*time.Second)
}

// updateInstalledPath atualiza uma instalação já existente sem recriar nem
// apagar o serviço. Deve ser chamada somente pelo processo elevado.
func updateInstalledPath(exePath string) error {
	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("conectar ao SCM: %w", err)
	}
	defer m.Disconnect()
	s, err := m.OpenService(ServiceName)
	if err != nil {
		return fmt.Errorf("abrir servico existente: %w", err)
	}
	defer s.Close()
	cfg, err := s.Config()
	if err != nil {
		return fmt.Errorf("ler configuracao do servico: %w", err)
	}
	cfg.BinaryPathName = windows.EscapeArg(exePath) // aspas se houver espaço (evita "unquoted service path")
	cfg.StartType = mgr.StartAutomatic
	cfg.DelayedAutoStart = false
	cfg.DisplayName = serviceDisplay
	cfg.Description = serviceDesc
	if err := s.UpdateConfig(cfg); err != nil {
		return fmt.Errorf("atualizar configuracao do servico: %w", err)
	}
	return harden(s)
}

// waitState espera o serviço chegar ao estado (ou o timeout).
func waitState(to svc.State, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		st, err := Status()
		if err != nil {
			return err
		}
		if st == stateName(to) {
			return nil
		}
		if st == "parado" && to == svc.Running {
			return fmt.Errorf("o servico parou logo apos iniciar (veja o log em C:\\ProgramData\\PontoParColetor)")
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timeout esperando o servico ficar %s", stateName(to))
		}
		time.Sleep(300 * time.Millisecond)
	}
}

// SetAutoStart liga/desliga o "Iniciar com o Windows": o StartType do serviço
// (Automatic ↔ Manual) E o app da bandeja no logon. NÃO para nem remove o
// serviço. Rodando como LocalSystem (o próprio serviço, via painel) não pede
// UAC; rodando como usuário comum exige elevação.
func SetAutoStart(enable bool) error {
	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("conectar ao SCM: %w", err)
	}
	defer m.Disconnect()
	s, err := m.OpenService(ServiceName)
	if err != nil {
		return fmt.Errorf("servico %s nao esta instalado", ServiceName)
	}
	defer s.Close()
	cfg, err := s.Config()
	if err != nil {
		return fmt.Errorf("ler configuracao do servico: %w", err)
	}
	if enable {
		cfg.StartType = mgr.StartAutomatic
		cfg.DelayedAutoStart = false
	} else {
		cfg.StartType = mgr.StartManual
	}
	if err := s.UpdateConfig(cfg); err != nil {
		return fmt.Errorf("atualizar inicio automatico: %w", err)
	}
	if enable {
		if exe := strings.Trim(cfg.BinaryPathName, `"`); exe != "" {
			if err := autorun.Enable(exe); err != nil {
				return err
			}
		}
		return harden(s)
	}
	return autorun.Disable()
}

// IsAutoStart diz se o serviço está configurado para iniciar com o Windows.
// Erro se o serviço não estiver instalado. Só pede SERVICE_QUERY_CONFIG.
func IsAutoStart() (bool, error) {
	scm, h, err := openLimited(windows.SERVICE_QUERY_CONFIG)
	if err != nil {
		return false, fmt.Errorf("servico %s nao esta instalado ou inacessivel: %w", ServiceName, err)
	}
	defer windows.CloseServiceHandle(scm)
	s := &mgr.Service{Name: ServiceName, Handle: h}
	defer s.Close()
	cfg, err := s.Config()
	if err != nil {
		return false, fmt.Errorf("ler configuracao do servico: %w", err)
	}
	return cfg.StartType == mgr.StartAutomatic, nil
}

// Stop para o serviço e aguarda a transição para Stopped (com timeout).
func Stop() error {
	return controlService(svc.Stop, svc.Stopped)
}

func controlService(c svc.Cmd, to svc.State) error {
	m, err := mgr.Connect()
	if err != nil {
		return err
	}
	defer m.Disconnect()
	s, err := m.OpenService(ServiceName)
	if err != nil {
		return fmt.Errorf("nao foi possivel acessar o servico: %w", err)
	}
	defer s.Close()

	status, err := s.Control(c)
	if err != nil {
		return fmt.Errorf("nao foi possivel enviar controle=%d: %w", c, err)
	}
	// Timeout generoso porque o Stop pode aguardar o flush do coletor.
	timeout := time.Now().Add(30 * time.Second)
	for status.State != to {
		if timeout.Before(time.Now()) {
			return fmt.Errorf("timeout aguardando estado=%d", to)
		}
		time.Sleep(300 * time.Millisecond)
		if status, err = s.Query(); err != nil {
			return err
		}
	}
	return nil
}

// Status devolve uma descrição legível do estado atual do serviço. Funciona
// para qualquer usuário (só pede SERVICE_QUERY_STATUS).
func Status() (string, error) {
	scm, h, err := openLimited(windows.SERVICE_QUERY_STATUS)
	if err != nil {
		if err == windows.ERROR_SERVICE_DOES_NOT_EXIST {
			return "nao instalado", nil
		}
		return "", err
	}
	defer windows.CloseServiceHandle(scm)
	defer windows.CloseServiceHandle(h)
	var st windows.SERVICE_STATUS
	if err := windows.QueryServiceStatus(h, &st); err != nil {
		return "", err
	}
	return stateName(svc.State(st.CurrentState)), nil
}

func stateName(s svc.State) string {
	switch s {
	case svc.Stopped:
		return "parado"
	case svc.StartPending:
		return "iniciando"
	case svc.StopPending:
		return "parando"
	case svc.Running:
		return "rodando"
	case svc.ContinuePending:
		return "continuando"
	case svc.PausePending:
		return "pausando"
	case svc.Paused:
		return "pausado"
	default:
		return fmt.Sprintf("estado desconhecido (%d)", s)
	}
}
