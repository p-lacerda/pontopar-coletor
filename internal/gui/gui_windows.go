//go:build windows

// Package gui fornece a tela de operação do coletor para quem instala no PC
// do cliente. O trabalho de coleta continua sendo feito pelo serviço Windows;
// esta janela só administra a configuração e o serviço, evitando dois
// coletores concorrentes.
package gui

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/lxn/walk"
	"github.com/lxn/win"
	"github.com/p-lacerda/pontopar-coletor/internal/config"
	"github.com/p-lacerda/pontopar-coletor/internal/setup"
	"github.com/p-lacerda/pontopar-coletor/internal/winservice"
)

const title = "PontoPar Coletor"

// ShowStartupError mostra uma mensagem mesmo quando a própria janela principal
// não conseguiu ser criada. Sem isto, um binário windowsgui fecharia em silêncio
// se o config.json estivesse ausente ou inválido.
func ShowStartupError(err error) {
	message := "O PontoPar não conseguiu abrir.\n\n" + err.Error() + "\n\nConfira se o arquivo config.json está na mesma pasta do pontopar-coletor.exe."
	win.MessageBox(0, syscall.StringToUTF16Ptr(message), syscall.StringToUTF16Ptr(title), win.MB_OK|win.MB_ICONERROR)
}

// Run abre a tela. Fechar a janela a esconde na bandeja do Windows; o menu do
// ícone da bandeja oferece uma saída explícita para encerrar somente a tela.
// O serviço, se instalado, segue independente dessa tela.
func Run(version string) error {
	dir, err := setup.ActiveConfigDir()
	if err != nil {
		return fmt.Errorf("localizar diretorio do executavel: %w", err)
	}
	cfg, err := config.Load(dir)
	if err != nil {
		// Cópias recém-baixadas não têm config.json ao lado do exe. Abra a
		// interface com defaults para que o usuário possa preencher e salvar,
		// em vez de falhar silenciosamente antes de mostrar os campos.
		cfg = config.Default()
	}

	mw, err := walk.NewMainWindow()
	if err != nil {
		return err
	}
	mw.SetTitle(title)
	_ = mw.SetIcon(walk.IconApplication())
	if err := mw.SetLayout(walk.NewVBoxLayout()); err != nil {
		return err
	}
	if err := mw.SetSize(walk.Size{Width: 680, Height: 760}); err != nil {
		return err
	}

	header, err := walk.NewLabel(mw)
	if err != nil {
		return err
	}
	header.SetText("Conexão do relógio Control iD")

	form, err := walk.NewComposite(mw)
	if err != nil {
		return err
	}
	if err := form.SetLayout(walk.NewGridLayout()); err != nil {
		return err
	}

	ip, err := addLine(form, "IP do Control iD", cfg.DeviceIp, false, false)
	if err != nil {
		return err
	}
	port, err := addLine(form, "Porta web", strconv.Itoa(cfg.Port()), false, false)
	if err != nil {
		return err
	}
	login, err := addLine(form, "Usuário", cfg.Login, false, false)
	if err != nil {
		return err
	}
	password, err := addLine(form, "Senha", cfg.Password, true, false)
	if err != nil {
		return err
	}
	deviceID, err := addLine(form, "ID do aparelho no Thera (obrigatório)", strconv.FormatInt(cfg.DeviceIdInt(), 10), false, false)
	if err != nil {
		return err
	}
	poll, err := addLine(form, "Verificar a cada (segundos)", strconv.Itoa(cfg.Poll()), false, false)
	if err != nil {
		return err
	}
	theraBase, err := addLine(form, "Endereço do Thera", cfg.TheraBase, false, false)
	if err != nil {
		return err
	}
	secret, err := addLine(form, "Segredo do Thera", cfg.DeviceSecret, true, false)
	if err != nil {
		return err
	}
	distance, err := addLine(form, "Distância da face (cm)", strconv.FormatFloat(defaultDistance(cfg.Facial.IdentificationDistanceCm), 'f', 0, 64), false, false)
	if err != nil {
		return err
	}
	liveness, err := addLine(form, "Liveness rigoroso (1/0)", boolText(cfg.Facial.LivenessMode), false, false)
	if err != nil {
		return err
	}
	region, err := addLine(form, "Limitar à região da tela (1/0)", boolText(cfg.Facial.LimitDisplayRegion), false, false)
	if err != nil {
		return err
	}
	photo, err := addLine(form, "Enviar foto da batida (1/0)", boolText(cfg.Facial.EnablePhotoUpload), false, false)
	if err != nil {
		return err
	}
	terminalMode, err := addLine(form, "Modo do terminal (attendance/access)", terminalModeText(cfg.Facial.TerminalMode), false, false)
	if err != nil {
		return err
	}
	if _, err := addLine(form, "Atualizações", cfg.Update.Repo, false, true); err != nil {
		return err
	}

	status, err := walk.NewLabel(mw)
	if err != nil {
		return err
	}
	status.SetText("Serviço: verificando…")

	buttons, err := walk.NewComposite(mw)
	if err != nil {
		return err
	}
	if err := buttons.SetLayout(walk.NewHBoxLayout()); err != nil {
		return err
	}
	saveButton, err := walk.NewPushButton(buttons)
	if err != nil {
		return err
	}
	saveButton.SetText("Salvar configuração")
	testButton, err := walk.NewPushButton(buttons)
	if err != nil {
		return err
	}
	testButton.SetText("Testar conexão")
	startButton, err := walk.NewPushButton(buttons)
	if err != nil {
		return err
	}
	startButton.SetText("Iniciar serviço")
	stopButton, err := walk.NewPushButton(buttons)
	if err != nil {
		return err
	}
	stopButton.SetText("Parar serviço")

	autoButton, err := walk.NewPushButton(mw)
	if err != nil {
		return err
	}
	autoButton.SetText("Assistente: instalar / desinstalar")
	logsButton, err := walk.NewPushButton(mw)
	if err != nil {
		return err
	}
	logsButton.SetText("Abrir arquivo de log")

	readFields := func() (*config.Config, error) {
		updated := *cfg
		updated.DeviceIp = strings.TrimSpace(ip.Text())
		updated.Login = strings.TrimSpace(login.Text())
		updated.Password = password.Text()
		updated.TheraBase = strings.TrimSpace(theraBase.Text())
		updated.DeviceSecret = strings.TrimSpace(secret.Text())
		updated.Facial.IdentificationDistanceCm, err = parseDistance(distance.Text())
		if err != nil {
			return nil, err
		}
		if updated.Facial.LivenessMode, err = parseBoolFlag(liveness.Text(), "liveness"); err != nil {
			return nil, err
		}
		if updated.Facial.LimitDisplayRegion, err = parseBoolFlag(region.Text(), "região da tela"); err != nil {
			return nil, err
		}
		if updated.Facial.EnablePhotoUpload, err = parseBoolFlag(photo.Text(), "foto da batida"); err != nil {
			return nil, err
		}
		updated.Facial.TerminalMode, err = parseTerminalMode(terminalMode.Text())
		if err != nil {
			return nil, err
		}

		parsedPort, err := positiveInt(port.Text(), "porta web", 1, 65535)
		if err != nil {
			return nil, err
		}
		updated.DevicePort = parsedPort
		parsedDeviceID, err := nonNegativeInt64(deviceID.Text(), "ID do aparelho no Thera")
		if err != nil {
			return nil, err
		}
		updated.SetDeviceId(parsedDeviceID)
		parsedPoll, err := positiveInt(poll.Text(), "intervalo", 1, 86400)
		if err != nil {
			return nil, err
		}
		updated.PollSeconds = parsedPoll
		if err := updated.Validate(); err != nil {
			return nil, err
		}
		return &updated, nil
	}

	refreshStatus := func() {
		serviceStatus, err := winservice.Status()
		if err != nil {
			status.SetText("Serviço: não foi possível consultar — " + err.Error())
			return
		}
		status.SetText("Serviço: " + serviceStatus)
	}

	persist := func() error {
		updated, err := readFields()
		if err == nil {
			err = config.Save(dir, updated)
		}
		if err != nil {
			return err
		}
		cfg = updated
		return nil
	}

	saveButton.Clicked().Attach(func() {
		if err := persist(); err != nil {
			walk.MsgBox(mw, title, "Não foi possível salvar:\n"+err.Error(), walk.MsgBoxIconError)
			return
		}
		walk.MsgBox(mw, title, "Configuração salva. Reinicie o serviço para aplicar uma alteração enquanto ele estiver rodando.", walk.MsgBoxIconInformation)
	})

	testButton.Clicked().Attach(func() {
		updated, err := readFields()
		if err != nil {
			walk.MsgBox(mw, title, "Confira os dados:\n"+err.Error(), walk.MsgBoxIconError)
			return
		}
		address := net.JoinHostPort(updated.DeviceIp, strconv.Itoa(updated.Port()))
		connection, err := net.DialTimeout("tcp", address, 4*time.Second)
		if err != nil {
			walk.MsgBox(mw, title, "Não consegui alcançar o Control iD em "+address+".\n\n"+err.Error(), walk.MsgBoxIconError)
			return
		}
		_ = connection.Close()
		walk.MsgBox(mw, title, "Control iD encontrado em "+address+".", walk.MsgBoxIconInformation)
	})

	startButton.Clicked().Attach(func() {
		if err := winservice.Start(); err != nil {
			walk.MsgBox(mw, title, "Não foi possível iniciar o serviço:\n"+err.Error(), walk.MsgBoxIconError)
			return
		}
		refreshStatus()
	})

	stopButton.Clicked().Attach(func() {
		if err := winservice.Stop(); err != nil {
			walk.MsgBox(mw, title, "Não foi possível parar o serviço:\n"+err.Error(), walk.MsgBoxIconError)
			return
		}
		refreshStatus()
	})

	autoButton.Clicked().Attach(func() {
		if err := showSetupWizard(mw, version, dir, persist); err != nil {
			walk.MsgBox(mw, title, "Não foi possível abrir o assistente:\n"+err.Error(), walk.MsgBoxIconError)
		}
	})

	logsButton.Clicked().Attach(func() {
		logPath := filepath.Join(dir, "pontopar-coletor.log")
		if _, err := os.Stat(logPath); err != nil {
			walk.MsgBox(mw, title, "Ainda não há log. Inicie o serviço primeiro.", walk.MsgBoxIconInformation)
			return
		}
		if !win.ShellExecute(mw.Handle(), nil, syscall.StringToUTF16Ptr(logPath), nil, syscall.StringToUTF16Ptr(dir), win.SW_SHOWNORMAL) {
			walk.MsgBox(mw, title, "O Windows não conseguiu abrir o arquivo de log.", walk.MsgBoxIconError)
		}
	})

	ni, err := walk.NewNotifyIcon(mw)
	if err != nil {
		return err
	}
	defer ni.Dispose()
	_ = ni.SetIcon(walk.IconApplication())
	_ = ni.SetToolTip(title + " " + version)
	_ = ni.SetVisible(true)

	var reallyExit bool
	showWindow := func() {
		mw.Show()
		mw.SetFocus()
		refreshStatus()
	}
	showAction := walk.NewAction()
	showAction.SetText("Abrir PontoPar")
	showAction.Triggered().Attach(showWindow)
	ni.ContextMenu().Actions().Add(showAction)
	exitAction := walk.NewAction()
	exitAction.SetText("Fechar janela")
	exitAction.Triggered().Attach(func() {
		reallyExit = true
		mw.Close()
	})
	ni.ContextMenu().Actions().Add(exitAction)
	ni.MouseUp().Attach(func(x, y int, button walk.MouseButton) {
		if button == walk.LeftButton {
			showWindow()
		}
	})
	mw.Closing().Attach(func(canceled *bool, reason walk.CloseReason) {
		if reallyExit {
			return
		}
		*canceled = true
		mw.Hide()
		_ = ni.ShowInfo(title, "O coletor continua disponível na bandeja do Windows.")
	})

	refreshStatus()
	mw.Show()
	mw.Run()
	return nil
}

func addLine(parent walk.Container, label, value string, password, readOnly bool) (*walk.LineEdit, error) {
	text, err := walk.NewLabel(parent)
	if err != nil {
		return nil, err
	}
	text.SetText(label)
	field, err := walk.NewLineEdit(parent)
	if err != nil {
		return nil, err
	}
	if err := field.SetText(value); err != nil {
		return nil, err
	}
	field.SetPasswordMode(password)
	if err := field.SetReadOnly(readOnly); err != nil {
		return nil, err
	}
	return field, nil
}

func defaultDistance(v float64) float64 {
	if v < 30 || v > 200 {
		return 50
	}
	return v
}
func boolText(v bool) string {
	if v {
		return "1"
	}
	return "0"
}
func parseDistance(raw string) (float64, error) {
	v, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
	if err != nil || v < 30 || v > 200 {
		return 0, fmt.Errorf("distância da face deve estar entre 30 e 200 cm")
	}
	return v, nil
}
func parseBoolFlag(raw, name string) (bool, error) {
	switch strings.TrimSpace(raw) {
	case "1":
		return true, nil
	case "0":
		return false, nil
	}
	return false, fmt.Errorf("%s deve ser 1 ou 0", name)
}

func terminalModeText(raw string) string {
	if strings.EqualFold(strings.TrimSpace(raw), "access") {
		return "access"
	}
	return "attendance"
}

func parseTerminalMode(raw string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "attendance", "ponto":
		return "attendance", nil
	case "access", "acesso":
		return "access", nil
	default:
		return "", fmt.Errorf("modo do terminal deve ser attendance ou access")
	}
}

func positiveInt(raw, name string, min, max int) (int, error) {
	n, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || n < min || n > max {
		return 0, fmt.Errorf("%s deve ser um número entre %d e %d", name, min, max)
	}
	return n, nil
}

func nonNegativeInt64(raw, name string) (int64, error) {
	n, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("%s deve ser zero ou um número inteiro positivo", name)
	}
	return n, nil
}

// showSetupWizard concentra a operação que precisa de administrador em uma
// tela simples. O processo elevado faz a cópia/serviço e mostra o resultado.
func showSetupWizard(owner *walk.MainWindow, version, configDir string, persist func() error) error {
	dlg, err := walk.NewDialogWithFixedSize(owner)
	if err != nil {
		return err
	}
	dlg.SetTitle("Assistente de instalação PontoPar")
	if err := dlg.SetLayout(walk.NewVBoxLayout()); err != nil {
		return err
	}
	if err := dlg.SetSize(walk.Size{Width: 540, Height: 315}); err != nil {
		return err
	}

	header, err := walk.NewLabel(dlg)
	if err != nil {
		return err
	}
	header.SetText("PontoPar Coletor " + version)
	description, err := walk.NewLabel(dlg)
	if err != nil {
		return err
	}
	description.SetText("Instalar coloca o coletor em uma pasta fixa do Windows, inicia agora e deixa a coleta automática após cada reinício — mesmo sem ninguém conectado na PC.")
	destination, err := walk.NewLabel(dlg)
	if err != nil {
		return err
	}
	destination.SetText("Pasta de instalação: " + setup.InstallDir())
	serviceStatus, err := winservice.Status()
	if err != nil {
		serviceStatus = "não foi possível consultar"
	}
	status, err := walk.NewLabel(dlg)
	if err != nil {
		return err
	}
	status.SetText("Estado atual do serviço: " + serviceStatus)
	note, err := walk.NewLabel(dlg)
	if err != nil {
		return err
	}
	note.SetText("Desinstalar para a coleta e remove o serviço, mas mantém configuração e registros. Assim, nenhum dado de ponto é apagado por engano.")

	buttons, err := walk.NewComposite(dlg)
	if err != nil {
		return err
	}
	if err := buttons.SetLayout(walk.NewHBoxLayout()); err != nil {
		return err
	}
	installButton, err := walk.NewPushButton(buttons)
	if err != nil {
		return err
	}
	installButton.SetText("Instalar e iniciar")
	uninstallButton, err := walk.NewPushButton(buttons)
	if err != nil {
		return err
	}
	uninstallButton.SetText("Desinstalar serviço")
	closeButton, err := walk.NewPushButton(buttons)
	if err != nil {
		return err
	}
	closeButton.SetText("Fechar")

	installButton.Clicked().Attach(func() {
		if err := persist(); err != nil {
			walk.MsgBox(dlg, title, "Confira e salve uma configuração válida:\n"+err.Error(), walk.MsgBoxIconError)
			return
		}
		answer := walk.MsgBox(dlg, "Instalar PontoPar", "O Windows pedirá autorização de administrador.\n\nDepois de aceitar, o coletor será copiado para a pasta fixa, iniciado agora e configurado para iniciar com o Windows.\n\nContinuar?", walk.MsgBoxYesNo|walk.MsgBoxIconQuestion)
		if answer != walk.DlgCmdYes {
			return
		}
		if err := runElevated(dlg, "setup-install", configDir); err != nil {
			walk.MsgBox(dlg, title, err.Error(), walk.MsgBoxIconError)
			return
		}
		walk.MsgBox(dlg, title, "Aceite a confirmação do Windows. O resultado aparecerá em seguida.", walk.MsgBoxIconInformation)
	})

	uninstallButton.Clicked().Attach(func() {
		answer := walk.MsgBox(dlg, "Desinstalar PontoPar", "Isso vai parar a coleta e remover o início automático.\n\nA configuração, o cursor e os registros serão mantidos para não apagar dados de ponto.\n\nContinuar?", walk.MsgBoxYesNo|walk.MsgBoxIconWarning)
		if answer != walk.DlgCmdYes {
			return
		}
		if err := runElevated(dlg, "setup-uninstall", configDir); err != nil {
			walk.MsgBox(dlg, title, err.Error(), walk.MsgBoxIconError)
			return
		}
		walk.MsgBox(dlg, title, "Aceite a confirmação do Windows. O resultado aparecerá em seguida.", walk.MsgBoxIconInformation)
	})
	closeButton.Clicked().Attach(func() { dlg.Close(walk.DlgCmdCancel) })
	dlg.Run()
	return nil
}

func runElevated(owner walk.Form, command, workingDir string) error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("não foi possível localizar o programa: %w", err)
	}
	if !win.ShellExecute(owner.Handle(), syscall.StringToUTF16Ptr("runas"), syscall.StringToUTF16Ptr(exe), syscall.StringToUTF16Ptr(command), syscall.StringToUTF16Ptr(workingDir), win.SW_SHOWNORMAL) {
		return fmt.Errorf("o Windows não abriu o pedido de administrador; execute o programa como administrador e tente novamente")
	}
	return nil
}
