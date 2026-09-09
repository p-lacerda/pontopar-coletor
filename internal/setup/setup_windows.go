//go:build windows

// Package setup instala o coletor numa pasta estável e administra o serviço
// Windows. O código roda apenas após a confirmação UAC acionada pela interface.
package setup

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/lxn/win"
	"github.com/p-lacerda/pontopar-coletor/internal/config"
	"github.com/p-lacerda/pontopar-coletor/internal/winservice"
)

const folderName = "PontoParColetor"

// InstallDir é a única pasta de produção do coletor. Não usamos Downloads:
// arquivos baixados podem ser movidos ou limpos e quebrariam o serviço no boot.
func InstallDir() string {
	base := strings.TrimSpace(os.Getenv("ProgramData"))
	if base == "" {
		base = `C:\ProgramData`
	}
	return filepath.Join(base, folderName)
}

// InstalledExePath devolve o caminho do binário que o serviço executa.
func InstalledExePath() string { return filepath.Join(InstallDir(), "pontopar-coletor.exe") }

// ActiveConfigDir escolhe a configuração instalada quando ela existe. Assim,
// abrir uma cópia antiga no Downloads ainda administra o coletor instalado.
func ActiveConfigDir() (string, error) {
	installed := InstallDir()
	if _, err := os.Stat(config.Path(installed)); err == nil {
		return installed, nil
	}
	return config.ExeDir()
}

// Install copia o binário e a configuração inicial para ProgramData e registra
// o serviço com auto-start. Uma configuração existente nunca é sobrescrita:
// isso protege segredo, cursor e o histórico operacional numa reinstalação.
func Install() error {
	sourceExe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("localizar programa de origem: %w", err)
	}
	sourceDir := filepath.Dir(sourceExe)
	sourceCfg, err := config.Load(sourceDir)
	if err != nil {
		return fmt.Errorf("configuracao da origem: %w", err)
	}

	destination := InstallDir()
	targetExe := InstalledExePath()
	if samePath(sourceExe, targetExe) {
		return winservice.InstallAndStart(targetExe)
	}

	// Se estamos atualizando uma cópia já instalada, libera o arquivo antes de
	// copiá-lo. A parada pede flush ao coletor, portanto não perde batidas.
	st, err := winservice.Status()
	if err != nil {
		return fmt.Errorf("consultar servico atual: %w", err)
	}
	if st != "nao instalado" && st != "parado" {
		if err := winservice.Stop(); err != nil {
			return fmt.Errorf("parar coletor antes de atualizar: %w", err)
		}
	}

	if err := os.MkdirAll(destination, 0o750); err != nil {
		return fmt.Errorf("criar pasta de instalacao: %w", err)
	}
	configPath := config.Path(destination)
	if _, err := os.Stat(configPath); err == nil {
		if _, err := config.Load(destination); err != nil {
			return fmt.Errorf("configuracao instalada invalida: %w", err)
		}
	} else if os.IsNotExist(err) {
		if err := config.Save(destination, sourceCfg); err != nil {
			return fmt.Errorf("copiar configuracao inicial: %w", err)
		}
	} else {
		return fmt.Errorf("verificar configuracao instalada: %w", err)
	}

	if err := copyExecutable(sourceExe, targetExe); err != nil {
		return err
	}
	if err := winservice.InstallAndStart(targetExe); err != nil {
		return err
	}
	return nil
}

// Uninstall remove somente o serviço e para a coleta. Configuração, cursor e
// logs ficam preservados em ProgramData para não apagar dados de ponto sem uma
// confirmação específica do administrador.
func Uninstall() error {
	st, err := winservice.Status()
	if err != nil {
		return fmt.Errorf("consultar servico: %w", err)
	}
	if st == "nao instalado" {
		return nil
	}
	if st != "parado" {
		if err := winservice.Stop(); err != nil {
			return fmt.Errorf("parar coletor: %w", err)
		}
	}
	if err := winservice.Uninstall(); err != nil {
		return err
	}
	return nil
}

// ShowResult apresenta o resultado no próprio processo elevado. A interface
// que disparou o UAC não recebe o código de saída do ShellExecute.
func ShowResult(title, message string, isError bool) {
	flags := uint32(win.MB_OK | win.MB_ICONINFORMATION)
	if isError {
		flags = win.MB_OK | win.MB_ICONERROR
	}
	win.MessageBox(0, syscall.StringToUTF16Ptr(message), syscall.StringToUTF16Ptr(title), flags)
}

func copyExecutable(source, target string) error {
	in, err := os.Open(source)
	if err != nil {
		return fmt.Errorf("abrir programa de origem: %w", err)
	}
	defer in.Close()

	tmp, err := os.CreateTemp(filepath.Dir(target), "pontopar-coletor.exe.tmp-*")
	if err != nil {
		return fmt.Errorf("criar programa temporario: %w", err)
	}
	tmpPath := tmp.Name()
	ok := false
	defer func() {
		if !ok {
			_ = os.Remove(tmpPath)
		}
	}()
	if _, err := io.Copy(tmp, in); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("copiar programa: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sincronizar programa: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("fechar programa temporario: %w", err)
	}
	if err := os.Rename(tmpPath, target); err != nil {
		return fmt.Errorf("ativar novo programa: %w", err)
	}
	ok = true
	return nil
}

func samePath(a, b string) bool {
	aa, errA := filepath.Abs(a)
	bb, errB := filepath.Abs(b)
	if errA != nil || errB != nil {
		return strings.EqualFold(filepath.Clean(a), filepath.Clean(b))
	}
	return strings.EqualFold(filepath.Clean(aa), filepath.Clean(bb))
}
