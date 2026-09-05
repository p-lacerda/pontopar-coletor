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
	"fmt"
	"time"

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
		StartType:   mgr.StartAutomatic, // auto-start no boot
		// DelayedAutoStart: true, // opcional: esperar rede/serviços subirem
	})
	if err != nil {
		return fmt.Errorf("criar servico: %w", err)
	}
	defer s.Close()

	// Recovery: reinicia nas 3 primeiras falhas; zera o contador após 1 dia.
	// Também é o mecanismo que reinicia o serviço após um auto-update (saída !=0).
	err = s.SetRecoveryActions([]mgr.RecoveryAction{
		{Type: mgr.ServiceRestart, Delay: 5 * time.Second},
		{Type: mgr.ServiceRestart, Delay: 15 * time.Second},
		{Type: mgr.ServiceRestart, Delay: 60 * time.Second},
	}, uint32((24 * time.Hour).Seconds())) // resetPeriod em SEGUNDOS
	if err != nil {
		_ = s.Delete()
		return fmt.Errorf("configurar recovery: %w", err)
	}

	// Fonte do event log (para elog.Info/Error do serviço).
	if err = eventlog.InstallAsEventCreate(ServiceName, eventlog.Error|eventlog.Warning|eventlog.Info); err != nil {
		_ = s.Delete()
		return fmt.Errorf("registrar fonte de eventos: %w", err)
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

	s, err := m.OpenService(ServiceName)
	if err != nil {
		return fmt.Errorf("servico %s nao esta instalado", ServiceName)
	}
	defer s.Close()

	if err = s.Delete(); err != nil {
		return fmt.Errorf("remover servico: %w", err)
	}
	if err = eventlog.Remove(ServiceName); err != nil {
		// Não fatal: o serviço já foi removido.
		return fmt.Errorf("servico removido, mas falhou ao remover fonte de eventos: %w", err)
	}
	return nil
}

// Start inicia o serviço via SCM.
func Start() error {
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
	if err := s.Start(); err != nil {
		return fmt.Errorf("nao foi possivel iniciar o servico: %w", err)
	}
	return nil
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

// Status devolve uma descrição legível do estado atual do serviço.
func Status() (string, error) {
	m, err := mgr.Connect()
	if err != nil {
		return "", err
	}
	defer m.Disconnect()
	s, err := m.OpenService(ServiceName)
	if err != nil {
		return "nao instalado", nil
	}
	defer s.Close()
	st, err := s.Query()
	if err != nil {
		return "", err
	}
	return stateName(st.State), nil
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
