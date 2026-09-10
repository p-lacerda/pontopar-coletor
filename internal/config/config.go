// Package config carrega e valida o config.json do coletor PontoPar.
//
// O arquivo de configuração fica SEMPRE ao lado do executável (não no
// diretório de trabalho), porque como serviço do Windows o cwd é
// C:\Windows\System32. Por isso resolvemos o diretório via os.Executable().
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// UpdateConfig controla o auto-update via GitHub Releases.
type UpdateConfig struct {
	// Repo no formato "owner/name" (ex.: "pontopar/pontopar-coletor").
	// Vazio desliga o auto-update.
	Repo string `json:"repo"`
	// CheckHours é o intervalo entre checagens de nova versão. Default 6h.
	CheckHours int `json:"checkHours"`
	// Token de acesso ao GitHub. Necessário apenas para repositório PRIVADO.
	// IMPORTANTE: o serviço roda como LocalSystem e NÃO herda o env do usuário,
	// então o token DEVE vir daqui (config), não da variável GITHUB_TOKEN.
	Token string `json:"token"`
}

// FacialConfig controla os parâmetros que o coletor aplica no iDFace quando
// recebe o manifesto do Thera. Os valores ficam no config local para também
// poderem ser ajustados pelo instalador sem manter fila em memória.
type FacialConfig struct {
	EnablePhotoUpload        bool    `json:"enablePhotoUpload"`
	LivenessMode             bool    `json:"livenessMode"`
	LimitDisplayRegion       bool    `json:"limitDisplayRegion"`
	IdentificationDistanceCm float64 `json:"identificationDistanceCm"`
}

// flexInt64 aceita, no JSON, tanto número quanto string (o config.json de
// referência tinha "deviceId": "" — string vazia). Guarda sempre como int64.
type flexInt64 int64

// UnmarshalJSON implementa json.Unmarshaler para flexInt64.
func (f *flexInt64) UnmarshalJSON(b []byte) error {
	s := strings.TrimSpace(string(b))
	if s == "null" || s == `""` || s == "" {
		*f = 0
		return nil
	}
	// Remove aspas se vier como string.
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		s = s[1 : len(s)-1]
	}
	s = strings.TrimSpace(s)
	if s == "" {
		*f = 0
		return nil
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return fmt.Errorf("deviceId invalido %q: %w", s, err)
	}
	*f = flexInt64(n)
	return nil
}

// Config espelha o config.json.
type Config struct {
	DeviceIp     string       `json:"deviceIp"`
	DevicePort   int          `json:"devicePort"` // default 80
	Login        string       `json:"login"`
	Password     string       `json:"password"`
	DeviceId     flexInt64    `json:"deviceId"`     // fallback quando o access_log não traz device_id
	DeviceSecret string       `json:"deviceSecret"` // vai na URL do /dao do Thera
	TheraBase    string       `json:"theraBase"`
	PollSeconds  int          `json:"pollSeconds"` // default 15
	Update       UpdateConfig `json:"update"`
	Facial       FacialConfig `json:"facial"`
}

// Default devolve uma configuração inicial segura para a interface. Ela não
// contém segredo nem credenciais válidas; o usuário precisa preencher e salvar
// antes de iniciar o serviço. Isso permite abrir o programa recém-baixado sem
// depender de um config.json externo.
func Default() *Config {
	return &Config{DeviceIp: "192.168.1.111", DevicePort: 90, Login: "admin", DeviceId: flexInt64(4409419584542362), PollSeconds: 15,
		TheraBase: "https://pediuai-api.debita.ai/thera",
		Facial:    FacialConfig{EnablePhotoUpload: true, LivenessMode: true, LimitDisplayRegion: true, IdentificationDistanceCm: 50},
		Update:    UpdateConfig{Repo: "p-lacerda/pontopar-coletor", CheckHours: 6}}
}

// Port devolve a porta do device (default 80).
func (c *Config) Port() int {
	if c.DevicePort == 0 {
		return 80
	}
	return c.DevicePort
}

// DeviceBase devolve a URL base HTTP do iDFace (linha Acesso, porta 80, sem TLS).
func (c *Config) DeviceBase() string {
	return fmt.Sprintf("http://%s:%d", c.DeviceIp, c.Port())
}

// DeviceIdInt devolve o deviceId de fallback como int64.
func (c *Config) DeviceIdInt() int64 { return int64(c.DeviceId) }

// SetDeviceId define o deviceId de fallback vindo da tela de configuração.
// Zero significa que o coletor deve usar o device_id vindo no access_log.
func (c *Config) SetDeviceId(value int64) { c.DeviceId = flexInt64(value) }

// TheraDaoURL monta a URL do webhook /dao do Thera.
// Espelha o Node: cfg.theraBase.replace(/\/$/,"") -> remove UMA barra final.
func (c *Config) TheraDaoURL() string {
	base := strings.TrimSuffix(c.TheraBase, "/")
	return base + "/api/controlid/notifications/" + c.DeviceSecret + "/dao"
}

// Poll devolve o intervalo de polling (default 15s).
func (c *Config) Poll() int {
	if c.PollSeconds == 0 {
		return 15
	}
	return c.PollSeconds
}

// UpdateCheckHours devolve o intervalo de checagem de update (default 6h).
func (c *Config) UpdateCheckHours() int {
	if c.Update.CheckHours == 0 {
		return 6
	}
	return c.Update.CheckHours
}

// ExeDir devolve o diretório onde o executável está, resolvendo symlinks.
// É o diretório onde ficam config.json, cursor.json e o log.
func ExeDir() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	// Resolve eventual symlink para achar o diretório real.
	if resolved, rerr := filepath.EvalSymlinks(exe); rerr == nil {
		exe = resolved
	}
	return filepath.Dir(exe), nil
}

// Path devolve o caminho padrão do config.json (ao lado do exe).
func Path(dir string) string { return filepath.Join(dir, "config.json") }

// Load lê e valida o config.json em <dir>/config.json.
func Load(dir string) (*Config, error) {
	p := Path(dir)
	data, err := os.ReadFile(p)
	if err != nil {
		return nil, fmt.Errorf("nao foi possivel ler %s: %w", p, err)
	}
	var c Config
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("config.json invalido (%s): %w", p, err)
	}
	if c.Facial.IdentificationDistanceCm == 0 {
		c.Facial = Default().Facial
	}
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

// Save grava a configuração em config.json com escrita atômica. Assim, uma queda
// de energia no meio do clique em "Salvar" não deixa o coletor sem configuração
// na próxima inicialização do Windows.
func Save(dir string, c *Config) error {
	if c == nil {
		return fmt.Errorf("configuracao ausente")
	}
	if err := c.Validate(); err != nil {
		return err
	}

	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return fmt.Errorf("serializar config.json: %w", err)
	}
	data = append(data, '\n')

	tmp, err := os.CreateTemp(dir, "config.json.tmp-*")
	if err != nil {
		return fmt.Errorf("criar config temporario: %w", err)
	}
	tmpPath := tmp.Name()
	ok := false
	defer func() {
		if !ok {
			_ = os.Remove(tmpPath)
		}
	}()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("proteger config temporario: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("gravar config temporario: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sincronizar config temporario: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("fechar config temporario: %w", err)
	}
	if err := os.Rename(tmpPath, Path(dir)); err != nil {
		return fmt.Errorf("substituir config.json: %w", err)
	}
	ok = true
	return nil
}

// Validate confere os campos obrigatórios.
func (c *Config) Validate() error {
	var missing []string
	if strings.TrimSpace(c.DeviceIp) == "" {
		missing = append(missing, "deviceIp")
	}
	if strings.TrimSpace(c.Login) == "" {
		missing = append(missing, "login")
	}
	// password pode ser vazio em alguns aparelhos, então não exigimos.
	if strings.TrimSpace(c.TheraBase) == "" {
		missing = append(missing, "theraBase")
	}
	if strings.TrimSpace(c.DeviceSecret) == "" {
		missing = append(missing, "deviceSecret")
	}
	if c.Facial.IdentificationDistanceCm != 0 && (c.Facial.IdentificationDistanceCm < 30 || c.Facial.IdentificationDistanceCm > 200) {
		return fmt.Errorf("identificationDistanceCm deve estar entre 30 e 200 cm")
	}
	if len(missing) > 0 {
		return fmt.Errorf("config.json incompleto, faltam campos: %s", strings.Join(missing, ", "))
	}
	return nil
}
