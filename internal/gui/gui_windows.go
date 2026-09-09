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
	"github.com/p-lacerda/pontopar-coletor/internal/winservice"
)

const title = "PontoPar Coletor"

// Run abre a tela. Fechar a janela a esconde na bandeja do Windows; o menu do
// ícone da bandeja oferece uma saída explícita para encerrar somente a tela.
// O serviço, se instalado, segue independente dessa tela.
func Run(version string) error {
	dir, err := config.ExeDir()
	if err != nil {
		return fmt.Errorf("localizar diretorio do executavel: %w", err)
	}
	cfg, err := config.Load(dir)
	if err != nil {
		return err
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
	if err := mw.SetSize(walk.Size{Width: 610, Height: 510}); err != nil {
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
	deviceID, err := addLine(form, "ID do aparelho no Thera", strconv.FormatInt(cfg.DeviceIdInt(), 10), false, false)
	if err != nil {
		return err
	}
	poll, err := addLine(form, "Verificar a cada (segundos)", strconv.Itoa(cfg.Poll()), false, false)
	if err != nil {
		return err
	}
	if _, err := addLine(form, "Endereço do Thera", cfg.TheraBase, false, true); err != nil {
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
	autoButton.SetText("Ativar início com o Windows")
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

	saveButton.Clicked().Attach(func() {
		updated, err := readFields()
		if err == nil {
			err = config.Save(dir, updated)
		}
		if err != nil {
			walk.MsgBox(mw, title, "Não foi possível salvar:\n"+err.Error(), walk.MsgBoxIconError)
			return
		}
		cfg = updated
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
		updated, err := readFields()
		if err == nil {
			err = config.Save(dir, updated)
		}
		if err != nil {
			walk.MsgBox(mw, title, "Salve uma configuração válida antes de ativar:\n"+err.Error(), walk.MsgBoxIconError)
			return
		}
		cfg = updated
		exe, err := os.Executable()
		if err != nil {
			walk.MsgBox(mw, title, "Não foi possível localizar o programa:\n"+err.Error(), walk.MsgBoxIconError)
			return
		}
		if !win.ShellExecute(mw.Handle(), syscall.StringToUTF16Ptr("runas"), syscall.StringToUTF16Ptr(exe), syscall.StringToUTF16Ptr("install-start"), syscall.StringToUTF16Ptr(dir), win.SW_SHOWNORMAL) {
			walk.MsgBox(mw, title, "O Windows não abriu o pedido de administrador. Execute o programa como administrador e tente novamente.", walk.MsgBoxIconError)
			return
		}
		walk.MsgBox(mw, title, "Aceite a confirmação do Windows. O coletor será registrado para iniciar automaticamente e iniciado agora.", walk.MsgBoxIconInformation)
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
