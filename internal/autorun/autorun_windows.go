//go:build windows

// Package autorun liga/desliga a abertura do app da bandeja ao entrar no
// Windows (para TODOS os usuários), via HKLM\...\Run.
//
// Por que isto existe: o serviço já sobe no boot (StartType=Automatic), mas ele
// roda na sessão 0, sem nada visível. Para quem está na frente do PC, "o app
// não iniciou com o Windows" — não havia ícone na bandeja, nem janela. Agora o
// instalador registra também o app da bandeja (`pontopar-coletor.exe tray`), que
// mostra o status e, se o serviço estiver parado, tenta subi-lo.
package autorun

import (
	"errors"
	"fmt"
	"strings"

	"golang.org/x/sys/windows/registry"
)

const (
	runKey      = `SOFTWARE\Microsoft\Windows\CurrentVersion\Run`
	approvedKey = `SOFTWARE\Microsoft\Windows\CurrentVersion\Explorer\StartupApproved\Run`
	valueName   = "PontoParColetor"
)

// Command monta a linha de comando registrada no Run.
func Command(exePath string) string { return `"` + exePath + `" tray` }

// Enable registra o app da bandeja para abrir no logon de qualquer usuário.
// Requer administrador (ou LocalSystem — o próprio serviço). Também limpa um
// "desabilitado" que o Gerenciador de Tarefas possa ter gravado.
func Enable(exePath string) error {
	k, _, err := registry.CreateKey(registry.LOCAL_MACHINE, runKey, registry.SET_VALUE)
	if err != nil {
		return fmt.Errorf("abrir chave Run: %w", err)
	}
	defer k.Close()
	if err := k.SetStringValue(valueName, Command(exePath)); err != nil {
		return fmt.Errorf("gravar inicialização do app: %w", err)
	}
	if ak, err := registry.OpenKey(registry.LOCAL_MACHINE, approvedKey, registry.SET_VALUE); err == nil {
		_ = ak.DeleteValue(valueName)
		ak.Close()
	}
	return nil
}

// Disable remove o app da bandeja do logon. Ausência não é erro.
func Disable() error {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, runKey, registry.SET_VALUE)
	if err != nil {
		if errors.Is(err, registry.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("abrir chave Run: %w", err)
	}
	defer k.Close()
	if err := k.DeleteValue(valueName); err != nil && !errors.Is(err, registry.ErrNotExist) {
		return fmt.Errorf("remover inicialização do app: %w", err)
	}
	return nil
}

// Registered diz se o valor Run existe (independente do Gerenciador de Tarefas).
func Registered() bool {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, runKey, registry.QUERY_VALUE)
	if err != nil {
		return false
	}
	defer k.Close()
	v, _, err := k.GetStringValue(valueName)
	return err == nil && strings.TrimSpace(v) != ""
}

// Enabled diz se o app da bandeja está registrado para abrir no logon (e não
// foi desabilitado pelo Gerenciador de Tarefas).
func Enabled() bool {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, runKey, registry.QUERY_VALUE)
	if err != nil {
		return false
	}
	defer k.Close()
	v, _, err := k.GetStringValue(valueName)
	if err != nil || strings.TrimSpace(v) == "" {
		return false
	}
	if ak, err := registry.OpenKey(registry.LOCAL_MACHINE, approvedKey, registry.QUERY_VALUE); err == nil {
		defer ak.Close()
		if b, _, err := ak.GetBinaryValue(valueName); err == nil && len(b) > 0 && b[0]&1 == 1 {
			return false // byte ímpar = desabilitado no Gerenciador de Tarefas
		}
	}
	return true
}
