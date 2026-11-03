// Comando pontopar-coletor: serviço Windows que coleta batidas do Control iD
// iDFace e encaminha ao Thera, com fila de reprocessamento (não-perda),
// auto-start no boot e auto-update via GitHub Releases.
//
// Subcomandos:
//
//	install    registra o serviço no SCM (auto-start + recovery). Requer Admin.
//	uninstall  remove o serviço. Requer Admin.
//	start      inicia o serviço.
//	stop       para o serviço.
//	status     mostra o estado do serviço.
//	run        roda em foreground (modo debug/interativo).
//	version    imprime a versão embutida.
//
// Sem argumentos, se estiver sob o SCM, roda como serviço; caso contrário,
// abre a interface gráfica de operação.
//
// A versão é injetada em build via: -ldflags "-X main.version=v1.2.3".
package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/p-lacerda/pontopar-coletor/internal/app"
	"github.com/p-lacerda/pontopar-coletor/internal/applog"
	"github.com/p-lacerda/pontopar-coletor/internal/audit"
	"github.com/p-lacerda/pontopar-coletor/internal/config"
	"github.com/p-lacerda/pontopar-coletor/internal/gui"
	"github.com/p-lacerda/pontopar-coletor/internal/setup"
	"github.com/p-lacerda/pontopar-coletor/internal/state"
	"github.com/p-lacerda/pontopar-coletor/internal/winservice"
)

// version é a versão embutida no build (ldflags). "dev" em builds locais.
var version = "dev"

func main() {
	// Determina se estamos rodando sob o SCM (serviço) ou interativo.
	inService, err := winservice.IsWindowsService()
	if err != nil {
		fmt.Fprintf(os.Stderr, "erro ao detectar contexto de servico: %v\n", err)
		os.Exit(1)
	}

	if inService {
		// Sob o SCM: roda como serviço (sem args do usuário).
		runService(false)
		return
	}

	// Modo interativo: despacha subcomando.
	args := os.Args[1:]
	if len(args) == 0 {
		runGUI()
		return
	}

	cmd := strings.ToLower(args[0])
	switch cmd {
	case "install":
		mustExe := mustExePath()
		if err := winservice.Install(mustExe); err != nil {
			fatalf("install: %v", err)
		}
		fmt.Printf("servico %q instalado (auto-start + recovery). Configure o config.json ao lado do exe e rode: %s start\n",
			winservice.ServiceName, exeBase())
	case "install-start":
		if err := winservice.InstallAndStart(mustExePath()); err != nil {
			fatalf("ativar inicio com Windows: %v", err)
		}
		fmt.Println("inicio automatico ativado; servico iniciado")
	case "setup-install":
		if err := setup.Install(); err != nil {
			setup.ShowResult("PontoPar — instalação", "Não foi possível instalar:\n"+err.Error(), true)
			os.Exit(1)
		}
		setup.ShowResult("PontoPar — instalação", "Instalação concluída. O coletor iniciou agora e também iniciará automaticamente com o Windows.", false)
	case "setup-uninstall":
		if err := setup.Uninstall(); err != nil {
			setup.ShowResult("PontoPar — desinstalação", "Não foi possível desinstalar:\n"+err.Error(), true)
			os.Exit(1)
		}
		setup.ShowResult("PontoPar — desinstalação", "O serviço foi removido e a coleta foi parada. Configuração e registros foram preservados em ProgramData.", false)
	case "uninstall", "remove":
		if err := winservice.Uninstall(); err != nil {
			fatalf("uninstall: %v", err)
		}
		fmt.Printf("servico %q removido\n", winservice.ServiceName)
	case "start":
		if err := winservice.Start(); err != nil {
			fatalf("start: %v", err)
		}
		fmt.Println("servico iniciado")
	case "stop":
		if err := winservice.Stop(); err != nil {
			fatalf("stop: %v", err)
		}
		fmt.Println("servico parado")
	case "status":
		st, err := winservice.Status()
		if err != nil {
			fatalf("status: %v", err)
		}
		fmt.Printf("servico %q: %s\n", winservice.ServiceName, st)
	case "run", "debug":
		// Foreground: útil para dev. debug=true no winservice usa debug.Run no
		// Windows; no Linux apenas executa a função de trabalho.
		runService(true)
	case "gui", "interface":
		runGUI()
	case "version", "--version", "-v":
		fmt.Println(version)
	case "help", "--help", "-h":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "subcomando desconhecido: %q\n\n", cmd)
		usage()
		os.Exit(2)
	}
}

// runGUI nunca deixa um erro de abertura invisível em builds windowsgui. Isso
// acontece, por exemplo, se alguém copiar apenas o .exe sem o config.json.
func runGUI() {
	if err := gui.Run(version); err != nil {
		gui.ShowStartupError(err)
		os.Exit(1)
	}
}

// runService monta o supervisor (coletor + painel web + recarga a quente) e o
// roda sob o SCM (interactive=false) ou em foreground (interactive=true).
func runService(interactive bool) {
	dir, err := config.ExeDir()
	if err != nil {
		fmt.Fprintf(os.Stderr, "nao foi possivel resolver o diretorio do executavel: %v\n", err)
		os.Exit(1)
	}

	// Log: sempre em arquivo ao lado do exe; espelha no stdout no modo interativo.
	logger := applog.Init(applog.DefaultPath(dir), interactive)
	defer logger.Close()

	// Estado vivo (para o painel) e auditoria persistente (prova de completude).
	st := state.New(dir, version)
	au := audit.New(dir)

	// O supervisor carrega a config, sobe o painel, roda o coletor e recarrega a
	// quente quando a config muda. Config inválida não derruba o serviço: o painel
	// sobe para o operador corrigir e salvar.
	sup := app.NewSupervisor(dir, version, interactive, logger, st, au)
	work := func(ctx context.Context) { sup.Run(ctx) }

	if interactive {
		// Foreground: roda direto com um contexto de fundo (Ctrl+C encerra o
		// processo; para shutdown limpo em dev, o SCM não está no caminho).
		logger.Infof("modo interativo (run) · versao=%s", version)
		work(context.Background())
		return
	}

	// Sob o SCM: winservice cuida do protocolo Stop/Shutdown e cancela o ctx.
	if err := winservice.RunAsService(work, false); err != nil {
		logger.Errorf("servico terminou com erro: %v", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Printf(`Coletor PontoPar %s

Uso:
  %s <subcomando>

Subcomandos:
  install     registra o servico no Windows (auto-start + recovery). Requer Administrador.
  install-start instala (se preciso), inicia agora e deixa no auto-start. Requer Administrador.
  uninstall   remove o servico. Requer Administrador.
  start       inicia o servico.
  stop        para o servico.
  status      mostra o estado do servico.
  run         roda em foreground (debug/teste), logando tambem no console.
  gui         abre a interface grafica (tambem e o padrao sem argumentos).
  version     imprime a versao.

A configuracao fica em config.json AO LADO do executavel.
`, version, exeBase())
}

func mustExePath() string {
	exe, err := os.Executable()
	if err != nil {
		fatalf("nao foi possivel resolver o caminho do executavel: %v", err)
	}
	return exe
}

func exeBase() string {
	exe, err := os.Executable()
	if err != nil {
		return "pontopar-coletor.exe"
	}
	// Só o nome do arquivo para exemplos de uso.
	for i := len(exe) - 1; i >= 0; i-- {
		if exe[i] == '/' || exe[i] == '\\' {
			return exe[i+1:]
		}
	}
	return exe
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
