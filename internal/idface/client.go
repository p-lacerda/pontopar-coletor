// Package idface fala com o aparelho Control iD iDFace (linha de ACESSO, HTTP
// porta 80, sem TLS). A sessão vai na query string das requisições, exatamente
// como a API .fcgi do aparelho espera.
//
// Este pacote é Go puro (net/http, encoding/json) — nada específico de SO.
package idface

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// FlexStr é uma string que, no JSON, aceita número OU string. O firmware do
// iDFace manda campos como número em algumas versões e como string em outras;
// isso replica o coalescing String(x) do coletor Node original, guardando
// sempre como string.
type FlexStr string

// UnmarshalJSON implementa json.Unmarshaler para FlexStr.
func (f *FlexStr) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if len(b) == 0 || string(b) == "null" {
		*f = ""
		return nil
	}
	if b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		*f = FlexStr(s)
		return nil
	}
	// Número, bool, etc.: usa a representação textual crua.
	*f = FlexStr(strings.Trim(string(b), `"`))
	return nil
}

func (f FlexStr) String() string { return string(f) }

// AccessLogsBatchLimit é o tamanho do lote pedido ao aparelho em cada
// load_objects de access_logs. Exportado para o collector saber quando um lote
// veio "cheio" (len == limit) e, portanto, ainda há backlog para drenar.
const AccessLogsBatchLimit = 500

// AccessLog é uma batida (access_logs) do aparelho.
type AccessLog struct {
	Id         FlexStr `json:"id"`
	Time       FlexStr `json:"time"`
	Event      FlexStr `json:"event"`
	DeviceId   FlexStr `json:"device_id"`
	UserId     FlexStr `json:"user_id"`
	PortalId   FlexStr `json:"portal_id"`
	LogTypeId  FlexStr `json:"log_type_id"`
	Confidence FlexStr `json:"confidence"`
}

// IdNum devolve o id numérico da batida. Erro se não for parseável (evita
// corromper o cursor com NaN, ao contrário do Node que faria Number("x")=NaN).
func (l AccessLog) IdNum() (int64, error) {
	s := strings.TrimSpace(string(l.Id))
	if s == "" {
		return 0, fmt.Errorf("access_log sem id")
	}
	return strconv.ParseInt(s, 10, 64)
}

// DeviceIdNum devolve o device_id numérico da batida. Erro se ausente/inválido
// (o chamador usa isso para decidir entre o device_id da batida e o fallback
// da config).
func (l AccessLog) DeviceIdNum() (int64, error) {
	s := strings.TrimSpace(string(l.DeviceId))
	if s == "" {
		return 0, fmt.Errorf("access_log sem device_id")
	}
	return strconv.ParseInt(s, 10, 64)
}

// deviceUser é o registro de usuário do aparelho (para o de-para).
type deviceUser struct {
	Id           FlexStr `json:"id"`
	Registration FlexStr `json:"registration"`
}

// User é a representação mínima de um usuário do iDFace necessária para
// sincronização. O aparelho não possui um campo "enabled"; a validade é
// controlada por EndTime (0 = sem expiração).
type User struct {
	Id           int64
	Registration string
	Name         string
	BeginTime    int64
	EndTime      int64
	Enabled      bool
	Image        []byte // opcional; JPEG para cadastro facial
}

// UserSnapshot é retornado por ListUsers. ImageRegistered vem de
// user_list_images.fcgi e permite a UI mostrar se há face no aparelho sem
// expor o conteúdo biométrico.
type UserSnapshot struct {
	Id              int64
	Registration    string
	Name            string
	BeginTime       int64
	EndTime         int64
	Enabled         bool
	ImageRegistered bool
}

// Logger é a interface mínima de log usada pelo cliente.
type Logger interface {
	Infof(format string, args ...any)
	Errorf(format string, args ...any)
}

// Client é o cliente HTTP do iDFace.
type Client struct {
	base  string
	login string
	pass  string
	http  *http.Client
	log   Logger

	mu       sync.Mutex // protege session e regCache
	session  string
	regCache map[string]string // user_id -> registration ("" = consultado, sem matrícula)
}

// New cria um cliente do iDFace. httpClient deve ter timeout curto (LAN).
func New(base, login, password string, httpClient *http.Client, log Logger) *Client {
	return &Client{
		base:     base,
		login:    login,
		pass:     password,
		http:     httpClient,
		log:      log,
		regCache: make(map[string]string),
	}
}

// postJSON faz um POST JSON e devolve o corpo bruto da resposta.
func (c *Client) postJSON(ctx context.Context, endpoint string, body any) ([]byte, int, error) {
	var buf []byte
	var err error
	switch b := body.(type) {
	case string:
		buf = []byte(b)
	default:
		buf, err = json.Marshal(body)
		if err != nil {
			return nil, 0, err
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(buf))
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, resp.StatusCode, err
	}
	return data, resp.StatusCode, nil
}

// Session devolve a sessão atual (pode estar vazia).
func (c *Client) Session() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.session
}

// ClearSession zera a sessão, forçando relogin no próximo uso.
func (c *Client) ClearSession() {
	c.mu.Lock()
	c.session = ""
	c.mu.Unlock()
}

// Login autentica no aparelho e guarda a sessão. Espelha deviceLogin do Node.
func (c *Client) Login(ctx context.Context) error {
	body := map[string]string{"login": c.login, "password": c.pass}
	data, _, err := c.postJSON(ctx, c.base+"/login.fcgi", body)
	if err != nil {
		return fmt.Errorf("login no aparelho falhou: %w", err)
	}
	var out struct {
		Session string `json:"session"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return fmt.Errorf("login no aparelho falhou (resposta invalida): %s", strings.TrimSpace(string(data)))
	}
	if out.Session == "" {
		return fmt.Errorf("login no aparelho falhou: %s", strings.TrimSpace(string(data)))
	}
	c.mu.Lock()
	c.session = out.Session
	c.mu.Unlock()
	c.log.Infof("login OK no iDFace")
	return nil
}

// SessionValid verifica se a sessão atual ainda é válida. Qualquer erro
// (rede/JSON) devolve false (igual ao catch->false do Node).
func (c *Client) SessionValid(ctx context.Context) bool {
	sess := c.Session()
	if sess == "" {
		return false
	}
	endpoint := c.base + "/session_is_valid.fcgi?session=" + url.QueryEscape(sess)
	data, _, err := c.postJSON(ctx, endpoint, "{}")
	if err != nil {
		return false
	}
	var out struct {
		Valid bool `json:"session_is_valid"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return false
	}
	return out.Valid
}

// EnsureSession garante uma sessão válida, relogando se necessário.
func (c *Client) EnsureSession(ctx context.Context) error {
	if c.SessionValid(ctx) {
		return nil
	}
	return c.Login(ctx)
}

// SetFacialConfiguration aplica configurações idempotentes do iDFace. O
// servidor só envia esse pequeno objeto no manifesto; nada fica acumulado no
// processo do coletor.
func (c *Client) SetFacialConfiguration(ctx context.Context, enablePhotoUpload, livenessMode, limitDisplayRegion bool, identificationDistanceCm float64) error {
	toFlag := func(v bool) string {
		if v {
			return "1"
		}
		return "0"
	}
	face := map[string]string{"liveness_mode": toFlag(livenessMode), "limit_identification_to_display_region": toFlag(limitDisplayRegion)}
	if identificationDistanceCm >= 30 && identificationDistanceCm <= 200 {
		// O firmware recebe min_detect_bounds_width, não centímetros.
		face["min_detect_bounds_width"] = strconv.FormatFloat(11.6/identificationDistanceCm, 'f', 2, 64)
	}
	body := map[string]any{"monitor": map[string]string{"enable_photo_upload": toFlag(enablePhotoUpload)}, "face_id": face}
	_, status, err := c.postJSON(ctx, c.base+"/set_configuration.fcgi?session="+url.QueryEscape(c.Session()), body)
	if err != nil {
		return err
	}
	if status < 200 || status >= 300 {
		return fmt.Errorf("set_configuration respondeu %d", status)
	}
	return nil
}

// LoadNewAccessLogs busca as batidas novas: event=7 e id > cursor, ordenadas
// por id, limite AccessLogsBatchLimit. Espelha fetchNewLogs do Node.
//
// ATENÇÃO: no request ao device, os valores de id e event são NÚMEROS (não
// string). Só no /dao do Thera é que viram string.
func (c *Client) LoadNewAccessLogs(ctx context.Context, cursor int64) ([]AccessLog, error) {
	body := map[string]any{
		"object": "access_logs",
		"where": []map[string]any{
			{"object": "access_logs", "field": "id", "operator": ">", "value": cursor, "connector": ") AND ("},
			{"object": "access_logs", "field": "event", "operator": "=", "value": 7},
		},
		"order": []string{"id"},
		"limit": AccessLogsBatchLimit,
	}
	endpoint := c.base + "/load_objects.fcgi?session=" + url.QueryEscape(c.Session())
	data, status, err := c.postJSON(ctx, endpoint, body)
	if err != nil {
		return nil, err
	}
	if status < 200 || status >= 300 {
		return nil, fmt.Errorf("load_objects access_logs respondeu %d: %s", status, strings.TrimSpace(string(data)))
	}
	var out struct {
		AccessLogs []AccessLog `json:"access_logs"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("resposta invalida de access_logs: %w", err)
	}
	return out.AccessLogs, nil
}

// Registration devolve a matrícula em cache para um user_id (miss -> "").
func (c *Client) Registration(userId string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.regCache[userId]
}

// ResolveRegistrations traduz user_id (interno do aparelho) -> registration
// (matrícula), populando o cache em memória. Espelha resolveRegistrations do
// Node: consulta em lote só os ids ainda não cacheados; ids que não voltarem
// são marcados como "" para não reconsultar todo tick. Qualquer erro de
// rede/JSON é apenas logado (best-effort) — a batida segue sem registration e
// o Thera cai no fallback do ControlIdUserMap.
func (c *Client) ResolveRegistrations(ctx context.Context, userIds []string) {
	// Dedupe + filtra os que já estão no cache.
	c.mu.Lock()
	seen := make(map[string]struct{})
	var faltando []string
	for _, id := range userIds {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		if _, ok := c.regCache[id]; !ok {
			faltando = append(faltando, id)
		}
	}
	c.mu.Unlock()

	if len(faltando) == 0 {
		return
	}

	// Monta o where encadeado com ") OR (" a partir do 2º item.
	where := make([]map[string]any, 0, len(faltando))
	for i, id := range faltando {
		n, err := strconv.ParseInt(id, 10, 64)
		if err != nil {
			// user_id não-numérico: marca como sem matrícula e não inclui na query.
			c.mu.Lock()
			c.regCache[id] = ""
			c.mu.Unlock()
			continue
		}
		item := map[string]any{"object": "users", "field": "id", "operator": "=", "value": n}
		if i > 0 {
			item["connector"] = ") OR ("
		}
		where = append(where, item)
	}
	if len(where) == 0 {
		return
	}

	body := map[string]any{
		"object": "users",
		"fields": []string{"id", "registration"},
		"where":  where,
		"limit":  len(faltando),
	}
	endpoint := c.base + "/load_objects.fcgi?session=" + url.QueryEscape(c.Session())
	data, status, err := c.postJSON(ctx, endpoint, body)
	if err != nil {
		// Erro de REDE: o aparelho pode até ter o user. NÃO cacheamos "" (isso
		// deixaria o user permanentemente sem matrícula até reiniciar); apenas
		// logamos e retentamos no próximo tick.
		c.log.Infof("aviso: falha ao traduzir user_id->registration (segue sem, retenta depois): %v", err)
		return
	}
	if status < 200 || status >= 300 {
		// Status não-2xx: NÃO cacheamos "" (não é uma negativa confirmada do
		// aparelho, e sim uma falha de consulta). Se for sessão inválida
		// (401/403), limpamos a sessão para forçar re-login no próximo tick.
		if status == http.StatusUnauthorized || status == http.StatusForbidden {
			c.log.Infof("aviso: load_objects users respondeu %d (sessao invalida); limpando sessao para re-login", status)
			c.ClearSession()
		} else {
			c.log.Infof("aviso: load_objects users respondeu %d (segue sem, retenta depois)", status)
		}
		return
	}
	var out struct {
		Users []deviceUser `json:"users"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		// Resposta ilegível: NÃO cacheamos "" (não confirma ausência do user);
		// retenta no próximo tick.
		c.log.Infof("aviso: resposta invalida de users (segue sem, retenta depois): %v", err)
		return
	}

	// Chegou aqui = o aparelho respondeu 2xx com um corpo válido: agora sim
	// temos a lista CONFIRMADA de users. Só neste caminho é seguro marcar ""
	// para os ids que não voltaram (o aparelho confirmadamente não tem esse
	// user), evitando reconsultar todo tick.
	c.mu.Lock()
	for _, u := range out.Users {
		uid := strings.TrimSpace(string(u.Id))
		if uid == "" {
			continue
		}
		c.regCache[uid] = strings.TrimSpace(string(u.Registration))
	}
	// Ids que não voltaram: marca "consultado, sem matrícula" (negativa
	// confirmada pelo aparelho).
	for _, id := range faltando {
		if _, ok := c.regCache[id]; !ok {
			c.regCache[id] = ""
		}
	}
	c.mu.Unlock()
}

// ListUsers lista usuários e indica quais possuem face cadastrada. É uma
// operação somente de leitura, útil para conferência/relatório.
func (c *Client) ListUsers(ctx context.Context) ([]UserSnapshot, error) {
	if err := c.EnsureSession(ctx); err != nil {
		return nil, err
	}
	body := map[string]any{"object": "users", "fields": []string{"id", "registration", "name", "begin_time", "end_time"}, "limit": 10000}
	data, status, err := c.postJSON(ctx, c.base+"/load_objects.fcgi?session="+url.QueryEscape(c.Session()), body)
	if err != nil {
		return nil, fmt.Errorf("listar usuários: %w", err)
	}
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		c.ClearSession()
	}
	if status < 200 || status >= 300 {
		return nil, fmt.Errorf("listar usuários respondeu %d: %s", status, strings.TrimSpace(string(data)))
	}
	var out struct {
		Users []struct {
			Id           FlexStr `json:"id"`
			Registration FlexStr `json:"registration"`
			Name         string  `json:"name"`
			BeginTime    int64   `json:"begin_time"`
			EndTime      int64   `json:"end_time"`
		} `json:"users"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("resposta inválida de usuários: %w", err)
	}
	images, err := c.listImageIDs(ctx)
	if err != nil {
		return nil, err
	}
	result := make([]UserSnapshot, 0, len(out.Users))
	for _, u := range out.Users {
		id, err := strconv.ParseInt(string(u.Id), 10, 64)
		if err != nil {
			continue
		}
		_, has := images[id]
		enabled := u.EndTime == 0 || u.EndTime > time.Now().Unix()
		result = append(result, UserSnapshot{Id: id, Registration: strings.TrimSpace(string(u.Registration)), Name: u.Name, BeginTime: u.BeginTime, EndTime: u.EndTime, Enabled: enabled, ImageRegistered: has})
	}
	return result, nil
}

func (c *Client) listImageIDs(ctx context.Context) (map[int64]struct{}, error) {
	data, status, err := c.postJSON(ctx, c.base+"/user_list_images.fcgi?session="+url.QueryEscape(c.Session()), "{}")
	if err != nil {
		return nil, fmt.Errorf("listar faces: %w", err)
	}
	if status < 200 || status >= 300 {
		return nil, fmt.Errorf("listar faces respondeu %d: %s", status, strings.TrimSpace(string(data)))
	}
	var out struct {
		UserIDs []int64 `json:"user_ids"`
		// Firmware variants have returned the same information as an array
		// of objects instead of user_ids. Accept both formats; otherwise a
		// perfectly enrolled face is incorrectly reported as missing.
		Images []struct {
			UserID FlexStr `json:"user_id"`
			ID     FlexStr `json:"id"`
		} `json:"images"`
		UserImages []struct {
			UserID FlexStr `json:"user_id"`
		} `json:"user_images"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("resposta inválida de faces: %w", err)
	}
	result := make(map[int64]struct{}, len(out.UserIDs))
	for _, id := range out.UserIDs {
		result[id] = struct{}{}
	}
	for _, image := range out.Images {
		id, err := strconv.ParseInt(strings.TrimSpace(string(image.UserID)), 10, 64)
		if err == nil && id > 0 {
			result[id] = struct{}{}
		}
	}
	for _, image := range out.UserImages {
		id, err := strconv.ParseInt(strings.TrimSpace(string(image.UserID)), 10, 64)
		if err == nil && id > 0 {
			result[id] = struct{}{}
		}
	}
	return result, nil
}

// GetUserImage lê a foto JPEG cadastrada no aparelho, sem persistê-la localmente.
func (c *Client) GetUserImage(ctx context.Context, id int64) ([]byte, error) {
	if err := c.EnsureSession(ctx); err != nil {
		return nil, err
	}
	endpoint := c.base + "/user_get_image.fcgi?user_id=" + strconv.FormatInt(id, 10) + "&get_timestamp=0&session=" + url.QueryEscape(c.Session())
	data, status, err := c.getBytes(ctx, endpoint)
	if err != nil {
		return nil, err
	}
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		c.ClearSession()
	}
	if status < 200 || status >= 300 {
		return nil, fmt.Errorf("buscar face %d respondeu %d", id, status)
	}
	if len(data) == 0 || len(data) > 2<<20 {
		return nil, fmt.Errorf("face %d inválida", id)
	}
	return data, nil
}

// UpsertUsers cria ou atualiza usuários por matrícula. Usuários já existentes
// são encontrados no aparelho, portanto nunca há duplicação por retry.
// Usuários ativos têm EndTime=0; inativos recebem EndTime=agora (o firmware
// deixa de aceitá-los). Não remove usuários desconhecidos automaticamente.
func (c *Client) UpsertUsers(ctx context.Context, users []User, now int64) error {
	if len(users) == 0 {
		return nil
	}
	if now <= 0 {
		now = time.Now().Unix()
	}
	if err := c.EnsureSession(ctx); err != nil {
		return err
	}
	values := make([]map[string]any, 0, len(users))
	// Resolver uma vez evita que retries criem usuários duplicados: a API só
	// faz upsert quando o id está presente.
	existing, err := c.ListUsers(ctx)
	if err != nil {
		return err
	}
	byReg := make(map[string]int64, len(existing))
	for _, old := range existing {
		if old.Id != 0 && old.Registration != "" {
			byReg[old.Registration] = old.Id
		}
	}
	for _, u := range users {
		reg := strings.TrimSpace(u.Registration)
		name := strings.TrimSpace(u.Name)
		if reg == "" || name == "" {
			return fmt.Errorf("usuário sem matrícula ou nome")
		}
		end := u.EndTime
		if u.Enabled {
			end = 0
		} else if end == 0 {
			end = now
		}
		if end < 0 {
			end = 0
		}
		value := map[string]any{"registration": reg, "name": name, "begin_time": u.BeginTime, "end_time": end}
		if id := byReg[reg]; id != 0 {
			value["id"] = id
		}
		values = append(values, value)
	}
	body := map[string]any{"object": "users", "values": values}
	data, status, err := c.postJSON(ctx, c.base+"/create_or_modify_objects.fcgi?session="+url.QueryEscape(c.Session()), body)
	if err != nil {
		return fmt.Errorf("sincronizar usuários: %w", err)
	}
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		c.ClearSession()
	}
	if status < 200 || status >= 300 {
		return fmt.Errorf("sincronizar usuários respondeu %d: %s", status, strings.TrimSpace(string(data)))
	}
	// A API não retorna os IDs quando faz upsert; recarregamos por matrícula
	// apenas para cadastrar as imagens opcionais sem adivinhar IDs.
	if err := c.syncImages(ctx, users); err != nil {
		return err
	}
	return nil
}

func (c *Client) syncImages(ctx context.Context, users []User) error {
	for _, u := range users {
		if len(u.Image) == 0 {
			continue
		}
		id, err := c.findUserID(ctx, u.Registration)
		if err != nil {
			return err
		}
		endpoint := c.base + "/user_set_image.fcgi?user_id=" + strconv.FormatInt(id, 10) + "&timestamp=" + strconv.FormatInt(time.Now().Unix(), 10) + "&match=1&session=" + url.QueryEscape(c.Session())
		data, status, err := c.postBytes(ctx, endpoint, "image/jpeg", u.Image)
		if err != nil {
			return fmt.Errorf("cadastrar face %s: %w", u.Registration, err)
		}
		if status < 200 || status >= 300 {
			return fmt.Errorf("cadastrar face %s respondeu %d: %s", u.Registration, status, strings.TrimSpace(string(data)))
		}
	}
	return nil
}

func (c *Client) findUserID(ctx context.Context, registration string) (int64, error) {
	body := map[string]any{"object": "users", "fields": []string{"id", "registration"}, "where": []map[string]any{{"object": "users", "field": "registration", "operator": "=", "value": registration}}, "limit": 1}
	data, status, err := c.postJSON(ctx, c.base+"/load_objects.fcgi?session="+url.QueryEscape(c.Session()), body)
	if err != nil {
		return 0, err
	}
	if status < 200 || status >= 300 {
		return 0, fmt.Errorf("buscar usuário respondeu %d", status)
	}
	var out struct {
		Users []deviceUser `json:"users"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return 0, err
	}
	if len(out.Users) == 0 {
		return 0, fmt.Errorf("usuário %s não encontrado após sincronização", registration)
	}
	return strconv.ParseInt(string(out.Users[0].Id), 10, 64)
}

func (c *Client) postBytes(ctx context.Context, endpoint, contentType string, body []byte) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Content-Type", contentType)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	return data, resp.StatusCode, err
}

func (c *Client) getBytes(ctx context.Context, endpoint string) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, 0, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20+1))
	return data, resp.StatusCode, err
}
