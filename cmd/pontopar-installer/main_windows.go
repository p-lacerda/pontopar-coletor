//go:build windows

// PontoPar-Setup é o instalador separado do coletor. Ele é compilado com UAC
// obrigatório, embute o coletor e cria a entrada padrão de desinstalação do
// Windows. A configuração exibida aqui nunca é enviada ao repositório: a build
// privada de cada cliente pode embuti-la como valor inicial.
package main

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/lxn/walk"
	"github.com/lxn/win"
	"github.com/p-lacerda/pontopar-coletor/internal/winservice"
	"golang.org/x/sys/windows/registry"
)

// version é definido pela release com -ldflags.
var version = "dev"

//go:embed payload/pontopar-coletor.exe
var collectorPayload []byte

//go:embed payload/config.json
var defaultConfigJSON []byte

const (
	productName   = "PontoPar Coletor"
	setupName     = "PontoPar-Setup.exe"
	collectorName = "pontopar-coletor.exe"
	folderName    = "PontoParColetor"
	registryPath  = `SOFTWARE\Microsoft\Windows\CurrentVersion\Uninstall\PontoParColetor`
)

type updateConfig struct {
	Repo       string `json:"repo"`
	CheckHours int    `json:"checkHours"`
	Token      string `json:"token"`
}

// installerConfig não importa o tipo interno do coletor para permitir que o
// instalador leia também o config de exemplo, onde deviceId pode ser string.
type installerConfig struct {
	DeviceIP     string       `json:"deviceIp"`
	DevicePort   int          `json:"devicePort"`
	Login        string       `json:"login"`
	Password     string       `json:"password"`
	DeviceID     string       `json:"deviceId"`
	TheraBase    string       `json:"theraBase"`
	DeviceSecret string       `json:"deviceSecret"`
	PollSeconds  int          `json:"pollSeconds"`
	Update       updateConfig `json:"update"`
}

func main() {
	if len(os.Args) > 1 && strings.EqualFold(os.Args[1], "--uninstall") {
		runUninstaller()
		return
	}
	if err := runInstaller(); err != nil {
		showError("Não foi possível abrir o instalador", err)
		os.Exit(1)
	}
}

func runInstaller() error {
	cfg, err := loadInitialConfig()
	if err != nil {
		return err
	}

	mw, err := walk.NewMainWindow()
	if err != nil {
		return err
	}
	mw.SetTitle(productName + " — Instalador")
	_ = mw.SetIcon(walk.IconApplication())
	if err := mw.SetLayout(walk.NewVBoxLayout()); err != nil {
		return err
	}
	if err := mw.SetSize(walk.Size{Width: 620, Height: 320}); err != nil {
		return err
	}

	header, err := walk.NewLabel(mw)
	if err != nil {
		return err
	}
	header.SetText(productName + " " + version)
	intro, err := walk.NewLabel(mw)
	if err != nil {
		return err
	}
	intro.SetText("Clique em Configurações para informar ou alterar IP, credenciais e o Segredo do Thera. A chave fica salva somente nesta PC.")

	status, err := walk.NewLabel(mw)
	if err != nil {
		return err
	}
	refreshStatus := func() {
		st, err := winservice.Status()
		if err != nil {
			status.SetText("Serviço: não foi possível consultar — " + err.Error())
			return
		}
		configState := "configuração pendente"
		if cfg.validate() == nil {
			configState = "configuração salva"
		}
		status.SetText("Serviço: " + st + " · " + configState + " · destino: " + installDir())
	}
	refreshStatus()

	buttons, err := walk.NewComposite(mw)
	if err != nil {
		return err
	}
	if err := buttons.SetLayout(walk.NewHBoxLayout()); err != nil {
		return err
	}
	settingsButton, err := walk.NewPushButton(buttons)
	if err != nil {
		return err
	}
	settingsButton.SetText("Configurações do Control iD e Thera")
	installButton, err := walk.NewPushButton(buttons)
	if err != nil {
		return err
	}
	installButton.SetText("Instalar e iniciar")
	uninstallButton, err := walk.NewPushButton(buttons)
	if err != nil {
		return err
	}
	uninstallButton.SetText("Desinstalar")
	cancelButton, err := walk.NewPushButton(buttons)
	if err != nil {
		return err
	}
	cancelButton.SetText("Cancelar")

	settingsButton.Clicked().Attach(func() {
		updated, saved, err := showSettings(mw, cfg)
		if err != nil {
			walk.MsgBox(mw, productName, "Não foi possível abrir configurações:\n"+err.Error(), walk.MsgBoxIconError)
			return
		}
		if saved {
			cfg = updated
			refreshStatus()
		}
	})

	installButton.Clicked().Attach(func() {
		if err := cfg.validate(); err != nil {
			walk.MsgBox(mw, productName, "Abra Configurações e preencha os dados antes de instalar:\n"+err.Error(), walk.MsgBoxIconError)
			return
		}
		if walk.MsgBox(mw, "Instalar PontoPar", "O coletor será instalado em:\n"+installDir()+"\n\nEle iniciará agora e automaticamente toda vez que o Windows ligar. Continuar?", walk.MsgBoxYesNo|walk.MsgBoxIconQuestion) != walk.DlgCmdYes {
			return
		}
		if err := install(cfg); err != nil {
			walk.MsgBox(mw, productName, "A instalação falhou:\n"+err.Error(), walk.MsgBoxIconError)
			return
		}
		refreshStatus()
		walk.MsgBox(mw, productName, "Instalação concluída. O coletor já está rodando e continuará após reiniciar o Windows.", walk.MsgBoxIconInformation)
	})

	uninstallButton.Clicked().Attach(func() {
		if walk.MsgBox(mw, "Desinstalar PontoPar", "Isso vai parar a coleta e remover o início automático.\n\nConfiguração, cursor e logs serão preservados para não apagar registros de ponto. Continuar?", walk.MsgBoxYesNo|walk.MsgBoxIconWarning) != walk.DlgCmdYes {
			return
		}
		if err := uninstall(); err != nil {
			walk.MsgBox(mw, productName, "A desinstalação falhou:\n"+err.Error(), walk.MsgBoxIconError)
			return
		}
		refreshStatus()
		walk.MsgBox(mw, productName, "O serviço foi removido. Os dados foram mantidos em "+installDir()+".", walk.MsgBoxIconInformation)
	})
	cancelButton.Clicked().Attach(func() { mw.Close() })

	mw.Show()
	mw.Run()
	return nil
}

// showSettings contém todos os inputs de conexão em uma tela própria. O botão
// de salvar grava config.json em ProgramData imediatamente, mesmo antes de o
// serviço ser instalado, para que a chave não se perca ao fechar o Setup.
func showSettings(owner *walk.MainWindow, current installerConfig) (installerConfig, bool, error) {
	dlg, err := walk.NewDialogWithFixedSize(owner)
	if err != nil {
		return current, false, err
	}
	dlg.SetTitle("Configurações do Control iD e Thera")
	if err := dlg.SetLayout(walk.NewVBoxLayout()); err != nil {
		return current, false, err
	}
	if err := dlg.SetSize(walk.Size{Width: 650, Height: 580}); err != nil {
		return current, false, err
	}
	note, err := walk.NewLabel(dlg)
	if err != nil {
		return current, false, err
	}
	note.SetText("Preencha os dados e clique em Salvar configurações. O segredo será armazenado somente no config.json desta PC.")
	form, err := walk.NewComposite(dlg)
	if err != nil {
		return current, false, err
	}
	// Não use GridLayout aqui. Na API imperativa do Walk cada controle de um
	// Grid precisa receber SetRange manualmente; sem isso os controles existem,
	// mas ficam sem célula e não são desenhados. HBox/VBox atribui o espaço
	// automaticamente e mantém o formulário visível em qualquer escala de DPI.
	if err := form.SetLayout(walk.NewVBoxLayout()); err != nil {
		return current, false, err
	}
	ip, err := addSettingsLine(form, "IP do Control iD", current.DeviceIP, false, false)
	if err != nil {
		return current, false, err
	}
	port, err := addSettingsLine(form, "Porta web", strconv.Itoa(defaultInt(current.DevicePort, 80)), false, false)
	if err != nil {
		return current, false, err
	}
	login, err := addSettingsLine(form, "Usuário", current.Login, false, false)
	if err != nil {
		return current, false, err
	}
	password, err := addSettingsLine(form, "Senha", current.Password, true, false)
	if err != nil {
		return current, false, err
	}
	deviceID, err := addSettingsLine(form, "ID do aparelho no Thera (obrigatório)", current.DeviceID, false, false)
	if err != nil {
		return current, false, err
	}
	theraBase, err := addSettingsLine(form, "Endereço do Thera", current.TheraBase, false, false)
	if err != nil {
		return current, false, err
	}
	secret, err := addSettingsLine(form, "Segredo do Thera (obrigatório)", current.DeviceSecret, true, false)
	if err != nil {
		return current, false, err
	}
	poll, err := addSettingsLine(form, "Verificar a cada (segundos)", strconv.Itoa(defaultInt(current.PollSeconds, 15)), false, false)
	if err != nil {
		return current, false, err
	}
	if _, err := addSettingsLine(form, "Atualizações", current.Update.Repo, false, true); err != nil {
		return current, false, err
	}

	buttons, err := walk.NewComposite(dlg)
	if err != nil {
		return current, false, err
	}
	if err := buttons.SetLayout(walk.NewHBoxLayout()); err != nil {
		return current, false, err
	}
	saveButton, err := walk.NewPushButton(buttons)
	if err != nil {
		return current, false, err
	}
	saveButton.SetText("Salvar configurações")
	cancelButton, err := walk.NewPushButton(buttons)
	if err != nil {
		return current, false, err
	}
	cancelButton.SetText("Cancelar")

	result := current
	saved := false
	readFields := func() (installerConfig, error) {
		updated := current
		updated.DeviceIP = strings.TrimSpace(ip.Text())
		updated.Login = strings.TrimSpace(login.Text())
		updated.Password = password.Text()
		updated.DeviceID = strings.TrimSpace(deviceID.Text())
		updated.TheraBase = strings.TrimSpace(theraBase.Text())
		updated.DeviceSecret = strings.TrimSpace(secret.Text())
		var err error
		if updated.DevicePort, err = boundedInt(port.Text(), "porta web", 1, 65535); err != nil {
			return installerConfig{}, err
		}
		if updated.PollSeconds, err = boundedInt(poll.Text(), "intervalo", 1, 86400); err != nil {
			return installerConfig{}, err
		}
		if err := updated.validate(); err != nil {
			return installerConfig{}, err
		}
		return updated, nil
	}
	saveButton.Clicked().Attach(func() {
		updated, err := readFields()
		if err != nil {
			walk.MsgBox(dlg, productName, "Confira a configuração:\n"+err.Error(), walk.MsgBoxIconError)
			return
		}
		if err := saveDraft(updated); err != nil {
			walk.MsgBox(dlg, productName, "Não foi possível salvar:\n"+err.Error(), walk.MsgBoxIconError)
			return
		}
		result = updated
		saved = true
		walk.MsgBox(dlg, productName, "Configurações salvas nesta PC. Agora clique em Instalar e iniciar.", walk.MsgBoxIconInformation)
		dlg.Close(walk.DlgCmdOK)
	})
	cancelButton.Clicked().Attach(func() { dlg.Close(walk.DlgCmdCancel) })
	dlg.Run()
	return result, saved, nil
}

func runUninstaller() {
	if walk.MsgBox(nil, "Desinstalar PontoPar", "Isso vai parar a coleta e remover o início automático.\n\nConfiguração, cursor e logs serão preservados. Continuar?", walk.MsgBoxYesNo|walk.MsgBoxIconWarning) != walk.DlgCmdYes {
		return
	}
	if err := uninstall(); err != nil {
		showError("Não foi possível desinstalar", err)
		return
	}
	walk.MsgBox(nil, "PontoPar", "O serviço foi removido. Os dados foram mantidos em "+installDir()+".", walk.MsgBoxIconInformation)
}

func install(cfg installerConfig) error {
	if len(collectorPayload) < 2 || collectorPayload[0] != 'M' || collectorPayload[1] != 'Z' {
		return fmt.Errorf("pacote do coletor inválido; baixe um instalador oficial")
	}
	st, err := winservice.Status()
	if err != nil {
		return err
	}
	if st != "nao instalado" && st != "parado" {
		if err := winservice.Stop(); err != nil {
			return fmt.Errorf("parar versão anterior: %w", err)
		}
	}

	dir := installDir()
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("criar pasta de instalação: %w", err)
	}
	if err := writeAtomic(filepath.Join(dir, collectorName), collectorPayload); err != nil {
		return err
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("montar configuração: %w", err)
	}
	if err := writeAtomic(filepath.Join(dir, "config.json"), append(data, '\n')); err != nil {
		return err
	}
	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("localizar instalador: %w", err)
	}
	setupDest := filepath.Join(dir, setupName)
	if !samePath(self, setupDest) {
		if err := copyFileAtomic(self, setupDest); err != nil {
			return err
		}
	}
	if err := winservice.InstallAndStart(filepath.Join(dir, collectorName)); err != nil {
		return err
	}
	if err := registerUninstaller(setupDest); err != nil {
		return fmt.Errorf("registrar desinstalador do Windows: %w", err)
	}
	return nil
}

func uninstall() error {
	st, err := winservice.Status()
	if err != nil {
		return err
	}
	if st != "nao instalado" && st != "parado" {
		if err := winservice.Stop(); err != nil {
			return fmt.Errorf("parar coletor: %w", err)
		}
	}
	if st != "nao instalado" {
		if err := winservice.Uninstall(); err != nil {
			return err
		}
	}
	if err := registry.DeleteKey(registry.LOCAL_MACHINE, registryPath); err != nil && err != syscall.ERROR_FILE_NOT_FOUND {
		return fmt.Errorf("remover registro de desinstalação: %w", err)
	}
	return nil
}

func registerUninstaller(setupPath string) error {
	k, _, err := registry.CreateKey(registry.LOCAL_MACHINE, registryPath, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer k.Close()
	values := map[string]string{
		"DisplayName":          productName,
		"DisplayVersion":       version,
		"Publisher":            "PontoPar",
		"DisplayIcon":          setupPath,
		"InstallLocation":      installDir(),
		"UninstallString":      `"` + setupPath + `" --uninstall`,
		"QuietUninstallString": `"` + setupPath + `" --uninstall`,
	}
	for name, value := range values {
		if err := k.SetStringValue(name, value); err != nil {
			return err
		}
	}
	return nil
}

func loadDefaultConfig() (installerConfig, error) {
	var cfg installerConfig
	if err := json.Unmarshal(defaultConfigJSON, &cfg); err != nil {
		return installerConfig{}, fmt.Errorf("configuração embutida inválida: %w", err)
	}
	normalizeConfig(&cfg)
	return cfg, nil
}

// loadInitialConfig prefere uma configuração previamente salva nesta PC. Isso
// permite abrir o Setup de novo para trocar o segredo ou o IP sem redigitar.
func loadInitialConfig() (installerConfig, error) {
	defaults, err := loadDefaultConfig()
	if err != nil {
		return installerConfig{}, err
	}
	data, err := os.ReadFile(filepath.Join(installDir(), "config.json"))
	if os.IsNotExist(err) {
		return defaults, nil
	}
	if err != nil {
		return installerConfig{}, fmt.Errorf("ler configuração salva: %w", err)
	}
	var saved installerConfig
	if err := json.Unmarshal(data, &saved); err != nil {
		return installerConfig{}, fmt.Errorf("configuração salva inválida: %w", err)
	}
	normalizeConfig(&saved)
	return saved, nil
}

func normalizeConfig(cfg *installerConfig) {
	if cfg.DevicePort == 0 {
		cfg.DevicePort = 80
	}
	if cfg.PollSeconds == 0 {
		cfg.PollSeconds = 15
	}
	if cfg.Update.CheckHours == 0 {
		cfg.Update.CheckHours = 6
	}
	if cfg.Update.Repo == "" {
		cfg.Update.Repo = "p-lacerda/pontopar-coletor"
	}
}

// saveDraft armazena as escolhas feitas na tela de Configurações antes da
// instalação. O serviço ainda não é criado aqui.
func saveDraft(cfg installerConfig) error {
	if err := cfg.validate(); err != nil {
		return err
	}
	if err := os.MkdirAll(installDir(), 0o750); err != nil {
		return fmt.Errorf("criar pasta de configuração: %w", err)
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("montar configuração: %w", err)
	}
	if err := writeAtomic(filepath.Join(installDir(), "config.json"), append(data, '\n')); err != nil {
		return err
	}
	return nil
}

func (c installerConfig) validate() error {
	var missing []string
	if strings.TrimSpace(c.DeviceIP) == "" {
		missing = append(missing, "IP do Control iD")
	}
	if strings.TrimSpace(c.Login) == "" {
		missing = append(missing, "usuário")
	}
	if strings.TrimSpace(c.TheraBase) == "" {
		missing = append(missing, "endereço do Thera")
	}
	if strings.TrimSpace(c.DeviceSecret) == "" {
		missing = append(missing, "segredo do Thera")
	}
	deviceID := strings.TrimSpace(c.DeviceID)
	if deviceID == "" {
		missing = append(missing, "ID do aparelho no Thera")
	} else if n, err := strconv.ParseInt(deviceID, 10, 64); err != nil || n <= 0 {
		return fmt.Errorf("ID do aparelho no Thera deve ser um número inteiro positivo (ex.: 4409419584542362)")
	}
	if len(missing) > 0 {
		return fmt.Errorf("faltam: %s", strings.Join(missing, ", "))
	}
	return nil
}

func addSettingsLine(parent walk.Container, label, value string, password, readOnly bool) (*walk.LineEdit, error) {
	row, err := walk.NewComposite(parent)
	if err != nil {
		return nil, err
	}
	if err := row.SetLayout(walk.NewHBoxLayout()); err != nil {
		return nil, err
	}
	text, err := walk.NewLabel(row)
	if err != nil {
		return nil, err
	}
	text.SetText(label)
	if err := text.SetMinMaxSize(walk.Size{Width: 245, Height: 0}, walk.Size{Width: 245, Height: 0}); err != nil {
		return nil, err
	}
	field, err := walk.NewLineEdit(row)
	if err != nil {
		return nil, err
	}
	if err := field.SetMinMaxSize(walk.Size{Width: 330, Height: 0}, walk.Size{}); err != nil {
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

func boundedInt(raw, name string, min, max int) (int, error) {
	n, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || n < min || n > max {
		return 0, fmt.Errorf("%s deve ser um número entre %d e %d", name, min, max)
	}
	return n, nil
}

func defaultInt(value, fallback int) int {
	if value == 0 {
		return fallback
	}
	return value
}

func installDir() string {
	base := strings.TrimSpace(os.Getenv("ProgramData"))
	if base == "" {
		base = `C:\ProgramData`
	}
	return filepath.Join(base, folderName)
}

func writeAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("criar arquivo temporário: %w", err)
	}
	tmpPath := tmp.Name()
	ok := false
	defer func() {
		if !ok {
			_ = os.Remove(tmpPath)
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("gravar %s: %w", filepath.Base(path), err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sincronizar %s: %w", filepath.Base(path), err)
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("ativar %s: %w", filepath.Base(path), err)
	}
	ok = true
	return nil
}

func copyFileAtomic(source, target string) error {
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp, err := os.CreateTemp(filepath.Dir(target), filepath.Base(target)+".tmp-*")
	if err != nil {
		return err
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
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, target); err != nil {
		return err
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

func showError(title string, err error) {
	win.MessageBox(0, syscall.StringToUTF16Ptr(err.Error()), syscall.StringToUTF16Ptr(title), win.MB_OK|win.MB_ICONERROR)
}
