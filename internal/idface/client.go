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

// Logger é a interface mínima de log usada pelo cliente.
type Logger interface {
	Infof(format string, args ...any)
	Errorf(format string, args ...any)
}

// Client é o cliente HTTP do iDFace.
type Client struct {
	base   string
	login  string
	pass   string
	http   *http.Client
	log    Logger

	mu       sync.Mutex        // protege session e regCache
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

// LoadNewAccessLogs busca as batidas novas: event=7 e id > cursor, ordenadas
// por id, limite 500. Espelha fetchNewLogs do Node.
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
		"limit": 500,
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
		c.log.Infof("aviso: falha ao traduzir user_id->registration (segue sem): %v", err)
		return
	}
	if status < 200 || status >= 300 {
		c.log.Infof("aviso: load_objects users respondeu %d (segue sem)", status)
		return
	}
	var out struct {
		Users []deviceUser `json:"users"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		c.log.Infof("aviso: resposta invalida de users (segue sem): %v", err)
		return
	}

	c.mu.Lock()
	for _, u := range out.Users {
		uid := strings.TrimSpace(string(u.Id))
		if uid == "" {
			continue
		}
		c.regCache[uid] = strings.TrimSpace(string(u.Registration))
	}
	// Ids que não voltaram: marca "consultado, sem matrícula".
	for _, id := range faltando {
		if _, ok := c.regCache[id]; !ok {
			c.regCache[id] = ""
		}
	}
	c.mu.Unlock()
}
