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
	"time"
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
	// TerminalMode controla o modo do iDFace: attendance registra ponto de
	// qualquer usuário facial cadastrado; access aplica as regras de acesso do
	// terminal. Attendance é o padrão para não transformar uma escala ausente
	// em "acesso negado" e perder uma marcação.
	TerminalMode string `json:"terminalMode"`
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

// UIConfig controla o painel web local (localhost) de status/configuração.
type UIConfig struct {
	// Enabled liga/desliga o painel. Default: ligado.
	Enabled *bool `json:"enabled"`
	// Port é a porta do painel em 127.0.0.1. Default 47114.
	Port int `json:"port"`
	// Bind é o endereço de escuta. SEMPRE loopback por segurança; expor em
	// 0.0.0.0 abriria o painel para a rede inteira. Default 127.0.0.1.
	Bind string `json:"bind"`
}

// Config espelha o config.json.
type Config struct {
	DeviceIp     string    `json:"deviceIp"`
	DevicePort   int       `json:"devicePort"` // default 80
	Login        string    `json:"login"`
	Password     string    `json:"password"`
	DeviceId     flexInt64 `json:"deviceId"`     // fallback quando o access_log não traz device_id
	DeviceSecret string    `json:"deviceSecret"` // vai na URL do /dao do Thera
	TheraBase    string    `json:"theraBase"`
	PollSeconds  int       `json:"pollSeconds"` // default 15
	// BatchSize é o tamanho da página pedida ao aparelho em cada load_objects de
	// access_logs. Default 500. Entre 1 e 1000.
	BatchSize int `json:"batchSize"`
	// MaxRetries é quantas vezes uma MESMA batida é reenviada ao Thera dentro do
	// ciclo antes de desistir (e reprocessar no próximo tick). 0 = sem retry extra
	// (o loop normal já reprocessa). Default 4.
	MaxRetries int `json:"maxRetries"`
	// RetryBackoffSeconds é a base do backoff exponencial entre tentativas de
	// reenvio. Default 2s (2s, 4s, 8s, ... limitado a 30s).
	RetryBackoffSeconds int `json:"retryBackoffSeconds"`
	// ForwardBatch é quantas batidas vão num ÚNICO POST ao Thera (envelope
	// object_changes) no full-sync. Quanto maior, menos requests (evita o 429 do
	// limitador por IP). Default 200. Entre 1 e 1000.
	ForwardBatch int `json:"forwardBatch"`
	// RegistrationOverrides mapeia user_id (interno do aparelho, string) ->
	// matrícula, para forçar a tradução quando o usuário no aparelho não tem o
	// campo registration preenchido corretamente. Tem PRIORIDADE sobre o de-para
	// lido do aparelho.
	RegistrationOverrides map[string]string `json:"registrationOverrides,omitempty"`
	Update                UpdateConfig      `json:"update"`
	Facial                FacialConfig      `json:"facial"`
	UI                    UIConfig          `json:"ui"`
}

// Default devolve uma configuração inicial segura para a interface. Ela não
// contém segredo nem credenciais válidas; o usuário precisa preencher e salvar
// antes de iniciar o serviço. Isso permite abrir o programa recém-baixado sem
// depender de um config.json externo.
func Default() *Config {
	enabled := true
	return &Config{DeviceIp: "192.168.1.111", DevicePort: 90, Login: "admin", DeviceId: flexInt64(4409419584542362), PollSeconds: 15,
		BatchSize: 500, ForwardBatch: 200, MaxRetries: 4, RetryBackoffSeconds: 2,
		TheraBase: "https://pediuai-api.debita.ai/thera",
		Facial:    FacialConfig{EnablePhotoUpload: true, LivenessMode: true, LimitDisplayRegion: true, IdentificationDistanceCm: 50, TerminalMode: "attendance"},
		Update:    UpdateConfig{Repo: "p-lacerda/pontopar-coletor", CheckHours: 6},
		UI:        UIConfig{Enabled: &enabled, Port: 47114, Bind: "127.0.0.1"}}
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

// BatchSizeOr devolve o tamanho de lote (default 500, limitado a 1..1000).
func (c *Config) BatchSizeOr() int {
	if c.BatchSize <= 0 {
		return 500
	}
	if c.BatchSize > 1000 {
		return 1000
	}
	return c.BatchSize
}

// ForwardBatchOr devolve o nº de batidas por POST ao Thera no full-sync
// (default 200, limitado a 1..1000).
func (c *Config) ForwardBatchOr() int {
	if c.ForwardBatch <= 0 {
		return 200
	}
	if c.ForwardBatch > 1000 {
		return 1000
	}
	return c.ForwardBatch
}

// MaxRetriesOr devolve o número de retentativas por batida (default 4).
func (c *Config) MaxRetriesOr() int {
	if c.MaxRetries < 0 {
		return 0
	}
	if c.MaxRetries == 0 {
		return 4
	}
	return c.MaxRetries
}

// RetryBackoffBase devolve a base do backoff entre retentativas (default 2s).
func (c *Config) RetryBackoffBase() time.Duration {
	s := c.RetryBackoffSeconds
	if s <= 0 {
		s = 2
	}
	return time.Duration(s) * time.Second
}

// UIEnabled diz se o painel web local está ligado (default: ligado).
func (c *Config) UIEnabled() bool {
	if c.UI.Enabled == nil {
		return true
	}
	return *c.UI.Enabled
}

// UIPort devolve a porta do painel (default 47114).
func (c *Config) UIPort() int {
	if c.UI.Port <= 0 || c.UI.Port > 65535 {
		return 47114
	}
	return c.UI.Port
}

// UIBind devolve o endereço de escuta do painel. Força loopback por segurança:
// um painel administrativo nunca pode ficar exposto para a rede.
func (c *Config) UIBind() string {
	b := strings.TrimSpace(c.UI.Bind)
	if b == "" || b == "0.0.0.0" || b == "::" {
		return "127.0.0.1"
	}
	return b
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
	if c.Facial.TerminalMode != "" && c.Facial.TerminalMode != "attendance" && c.Facial.TerminalMode != "access" {
		return fmt.Errorf("terminalMode deve ser attendance ou access")
	}
	if c.BatchSize != 0 && (c.BatchSize < 1 || c.BatchSize > 1000) {
		return fmt.Errorf("batchSize deve estar entre 1 e 1000")
	}
	if c.ForwardBatch != 0 && (c.ForwardBatch < 1 || c.ForwardBatch > 1000) {
		return fmt.Errorf("forwardBatch deve estar entre 1 e 1000")
	}
	if c.UI.Port != 0 && (c.UI.Port < 1 || c.UI.Port > 65535) {
		return fmt.Errorf("ui.port deve estar entre 1 e 65535")
	}
	if len(missing) > 0 {
		return fmt.Errorf("config.json incompleto, faltam campos: %s", strings.Join(missing, ", "))
	}
	return nil
}

// InstallFile é o arquivo que o instalador (Inno Setup) grava com SÓ os campos
// do assistente (IP, credenciais, Thera, facial). O coletor o mescla sobre o
// config.json existente em vez de o instalador sobrescrever o arquivo inteiro —
// antes, cada reinstalação apagava a matrícula manual (batidas sem matrícula são
// descartadas no Thera) e zerava o token do auto-update.
const InstallFile = "install-config.json"

// ApplyInstallFile mescla <dir>/install-config.json sobre o config.json (ou
// sobre os defaults, numa instalação nova), grava com Save e apaga o arquivo.
// Devolve applied=false quando não há arquivo de instalação.
func ApplyInstallFile(dir string) (applied bool, err error) {
	ipath := filepath.Join(dir, InstallFile)
	data, err := os.ReadFile(ipath)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("ler %s: %w", InstallFile, err)
	}
	var patch map[string]any
	if err := json.Unmarshal(data, &patch); err != nil {
		return false, fmt.Errorf("%s inválido: %w", InstallFile, err)
	}
	base := map[string]any{}
	existing, rerr := os.ReadFile(Path(dir))
	if rerr != nil || json.Unmarshal(existing, &base) != nil {
		base = map[string]any{}
		def, _ := json.Marshal(Default())
		_ = json.Unmarshal(def, &base)
	}
	deepMerge(base, patch)
	merged, err := json.Marshal(base)
	if err != nil {
		return false, err
	}
	var c Config
	if err := json.Unmarshal(merged, &c); err != nil {
		return false, fmt.Errorf("configuração mesclada inválida: %w", err)
	}
	if c.Facial.IdentificationDistanceCm == 0 {
		c.Facial = Default().Facial
	}
	if err := Save(dir, &c); err != nil {
		return false, err
	}
	_ = os.Remove(ipath)
	return true, nil
}

// deepMerge copia patch sobre base; objetos aninhados são mesclados campo a campo.
func deepMerge(base, patch map[string]any) {
	for k, v := range patch {
		if pm, ok := v.(map[string]any); ok {
			if bm, ok := base[k].(map[string]any); ok {
				deepMerge(bm, pm)
				continue
			}
		}
		base[k] = v
	}
}
