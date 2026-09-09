//go:build !windows

// Stubs para plataformas não-Windows. Permitem compilar/vetar o restante do
// projeto em qualquer host (o alvo real é windows/amd64). No Linux estas
// funções não fazem nada útil além de reportar que o SO não é suportado.
package winservice

import (
	"context"
	"errors"
)

// ServiceName é o nome interno do serviço.
const ServiceName = "PontoParColetor"

// errNotWindows é devolvido pelas operações de serviço fora do Windows.
var errNotWindows = errors.New("gerenciamento de servico so e suportado no Windows")

// RunFunc é a função de trabalho do coletor.
type RunFunc func(ctx context.Context)

// IsWindowsService sempre false fora do Windows.
func IsWindowsService() (bool, error) { return false, nil }

// RunAsService roda a função de trabalho em foreground (não há SCM aqui).
// Útil para desenvolvimento no Linux: apenas executa run com um contexto de
// fundo. isDebug é ignorado.
func RunAsService(run RunFunc, isDebug bool) error {
	run(context.Background())
	return nil
}

// Install não é suportado fora do Windows.
func Install(exePath string) error { return errNotWindows }

// Uninstall não é suportado fora do Windows.
func Uninstall() error { return errNotWindows }

// Start não é suportado fora do Windows.
func Start() error { return errNotWindows }

// InstallAndStart não é suportado fora do Windows.
func InstallAndStart(exePath string) error { return errNotWindows }

// Stop não é suportado fora do Windows.
func Stop() error { return errNotWindows }

// Status não é suportado fora do Windows.
func Status() (string, error) { return "", errNotWindows }
