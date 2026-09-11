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

// ScheduleRule é uma regra semanal do Thera em minutos desde 00:00.
type ScheduleRule struct {
	Weekday          int
	Entrada          *int
	SaidaIntervalo   *int
	RetornoIntervalo *int
	Saida            *int
}

// Schedule é a jornada do Thera associada a uma ou mais matrículas.
type Schedule struct {
	ScheduleID            string
	Name                  string
	EmployeeRegistrations []string
	Rules                 []ScheduleRule
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

// SyncClock ajusta o relógio do iDFace para o instante informado.
//
// O endpoint pertence à API da linha Access/iDFace (não ao endpoint de
// configuração geral): ele espera os campos de data/hora separados e a sessão
// na query string. O coletor chama este método de forma best-effort, portanto
// uma falha de rede não interrompe a importação das batidas.
func (c *Client) SyncClock(ctx context.Context, now time.Time) error {
	if err := c.EnsureSession(ctx); err != nil {
		return err
	}
	body := map[string]int{
		"day": now.Day(), "month": int(now.Month()), "year": now.Year(),
		"hour": now.Hour(), "minute": now.Minute(), "second": now.Second(),
	}
	endpoint := c.base + "/set_system_time.fcgi?session=" + url.QueryEscape(c.Session())
	data, status, err := c.postJSON(ctx, endpoint, body)
	if err != nil {
		return fmt.Errorf("ajuste de horário no aparelho falhou: %w", err)
	}
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		c.ClearSession()
	}
	if status < 200 || status >= 300 {
		return fmt.Errorf("ajuste de horário no aparelho respondeu %d: %s", status, strings.TrimSpace(string(data)))
	}
	c.log.Infof("relógio do iDFace sincronizado: %s", now.Format("02/01/2006 15:04:05"))
	return nil
}

// SetFacialConfiguration aplica configurações idempotentes do iDFace. O
// servidor só envia esse pequeno objeto no manifesto; nada fica acumulado no
// processo do coletor.
func (c *Client) SetFacialConfiguration(ctx context.Context, enablePhotoUpload, livenessMode, limitDisplayRegion bool, identificationDistanceCm float64, attendanceMode string) error {
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
	mode := strings.ToLower(strings.TrimSpace(attendanceMode))
	if mode == "" {
		mode = "attendance"
	}
	if mode != "attendance" && mode != "access" {
		return fmt.Errorf("attendanceMode inválido %q (use attendance ou access)", attendanceMode)
	}
	// No modo attendance (attendance_mode=1), o Control iD registra ponto para
	// usuário facial cadastrado e não bloqueia a batida por uma escala ausente.
	// O modo access (attendance_mode=0) é opt-in e deixa o terminal avaliar as
	// access_rules/time_zones publicadas pelo coletor.
	body := map[string]any{
		"general":    map[string]string{"attendance_mode": map[string]string{"attendance": "1", "access": "0"}[mode]},
		"identifier": map[string]string{"log_type": "0"},
		"monitor":    map[string]string{"enable_photo_upload": toFlag(enablePhotoUpload)},
		"face_id":    face,
	}
	_, status, err := c.postJSON(ctx, c.base+"/set_configuration.fcgi?session="+url.QueryEscape(c.Session()), body)
	if err != nil {
		return err
	}
	if status < 200 || status >= 300 {
		return fmt.Errorf("set_configuration respondeu %d", status)
	}
	return nil
}

func (c *Client) loadObjects(ctx context.Context, object string, fields []string) ([]map[string]json.RawMessage, error) {
	body := map[string]any{"object": object, "fields": fields, "limit": 10000}
	data, status, err := c.postJSON(ctx, c.base+"/load_objects.fcgi?session="+url.QueryEscape(c.Session()), body)
	if err != nil {
		return nil, err
	}
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		c.ClearSession()
	}
	if status < 200 || status >= 300 {
		return nil, fmt.Errorf("listar %s respondeu %d: %s", object, status, strings.TrimSpace(string(data)))
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("resposta inválida de %s: %w", object, err)
	}
	var rows []map[string]json.RawMessage
	if v, ok := raw[object]; ok {
		if err := json.Unmarshal(v, &rows); err != nil {
			return nil, err
		}
	}
	return rows, nil
}

func flexRaw(v json.RawMessage) string {
	var f FlexStr
	if len(v) == 0 {
		return ""
	}
	if json.Unmarshal(v, &f) == nil {
		return strings.TrimSpace(string(f))
	}
	return ""
}

func (c *Client) createOrModifyObjects(ctx context.Context, object string, values []map[string]any) error {
	if len(values) == 0 {
		return nil
	}
	body := map[string]any{"object": object, "values": values}
	data, status, err := c.postJSON(ctx, c.base+"/create_or_modify_objects.fcgi?session="+url.QueryEscape(c.Session()), body)
	if err != nil {
		return err
	}
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		c.ClearSession()
	}
	if status < 200 || status >= 300 {
		return fmt.Errorf("gravar %s respondeu %d: %s", object, status, strings.TrimSpace(string(data)))
	}
	return nil
}

func (c *Client) destroyObjects(ctx context.Context, object string, where map[string]any) error {
	field, _ := where["field"].(string)
	operator, _ := where["operator"].(string)
	value := where["value"]
	if field == "" {
		return fmt.Errorf("filtro de %s sem campo", object)
	}
	condition := any(value)
	if operator != "" && operator != "=" && operator != "==" {
		condition = map[string]any{operator: value}
	}
	body := map[string]any{"object": object, "where": map[string]any{object: map[string]any{field: condition}}}
	data, status, err := c.postJSON(ctx, c.base+"/destroy_objects.fcgi?session="+url.QueryEscape(c.Session()), body)
	if err != nil {
		return err
	}
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		c.ClearSession()
	}
	if status < 200 || status >= 300 {
		return fmt.Errorf("limpar %s respondeu %d: %s", object, status, strings.TrimSpace(string(data)))
	}
	return nil
}

func minutesSeconds(v *int) int {
	if v == nil {
		return 0
	}
	if *v < 0 {
		return 0
	}
	if *v > 1439 {
		return 86399
	}
	return *v * 60
}

func dayFlags(day int) map[string]int {
	flags := map[string]int{"sun": 0, "mon": 0, "tue": 0, "wed": 0, "thu": 0, "fri": 0, "sat": 0, "hol1": 0, "hol2": 0, "hol3": 0}
	keys := []string{"sun", "mon", "tue", "wed", "thu", "fri", "sat"}
	if day >= 0 && day < len(keys) {
		flags[keys[day]] = 1
	}
	return flags
}

// ApplySchedules publica as jornadas como time_zones + access_rules e vincula
// cada matrícula a sua regra. É idempotente: objetos PontoPar são encontrados
// por nome, intervalos/relações da regra são recriados e usuários sem jornada
// ficam livres para não quebrar cadastros legados.
func (c *Client) ApplySchedules(ctx context.Context, schedules []Schedule) error {
	if len(schedules) == 0 {
		return nil
	}
	if err := c.EnsureSession(ctx); err != nil {
		return err
	}
	users, err := c.ListUsers(ctx)
	if err != nil {
		return err
	}
	byReg := make(map[string]int64, len(users))
	for _, u := range users {
		if u.Id > 0 && u.Registration != "" {
			byReg[u.Registration] = u.Id
		}
	}
	// O terminal é gerenciado pelo PontoPar: remove relações antigas de todos
	// os usuários antes de reconstruir as relações desejadas. Isso também limpa
	// a autorização de quem perdeu a escala no Thera.
	for _, uid := range byReg {
		if err := c.destroyObjects(ctx, "user_access_rules", map[string]any{"object": "user_access_rules", "field": "user_id", "operator": "=", "value": uid}); err != nil {
			return err
		}
	}
	tzRows, err := c.loadObjects(ctx, "time_zones", []string{"id", "name"})
	if err != nil {
		return err
	}
	ruleRows, err := c.loadObjects(ctx, "access_rules", []string{"id", "name", "type", "priority"})
	if err != nil {
		return err
	}
	portals, err := c.loadObjects(ctx, "portals", []string{"id"})
	if err != nil {
		return err
	}
	for _, schedule := range schedules {
		if strings.TrimSpace(schedule.ScheduleID) == "" {
			continue
		}
		prefix := "PontoPar:" + schedule.ScheduleID
		tzID := findNamedID(tzRows, prefix)
		if tzID == 0 {
			if err := c.createOrModifyObjects(ctx, "time_zones", []map[string]any{{"name": prefix + ":timezone"}}); err != nil {
				return err
			}
			tzRows, err = c.loadObjects(ctx, "time_zones", []string{"id", "name"})
			if err != nil {
				return err
			}
			tzID = findNamedID(tzRows, prefix)
		}
		if tzID == 0 {
			return fmt.Errorf("time_zone da escala %s não foi criado", schedule.Name)
		}
		ruleID := findNamedID(ruleRows, prefix)
		if ruleID == 0 {
			if err := c.createOrModifyObjects(ctx, "access_rules", []map[string]any{{"name": prefix + ":rule", "type": 1, "priority": 0}}); err != nil {
				return err
			}
			ruleRows, err = c.loadObjects(ctx, "access_rules", []string{"id", "name", "type", "priority"})
			if err != nil {
				return err
			}
			ruleID = findNamedID(ruleRows, prefix)
		}
		if ruleID == 0 {
			return fmt.Errorf("access_rule da escala %s não foi criada", schedule.Name)
		}
		// Remove somente a definição desta regra gerenciada pelo PontoPar.
		if err := c.destroyObjects(ctx, "time_spans", map[string]any{"object": "time_spans", "field": "time_zone_id", "operator": "=", "value": tzID}); err != nil {
			return err
		}
		if err := c.destroyObjects(ctx, "access_rule_time_zones", map[string]any{"object": "access_rule_time_zones", "field": "access_rule_id", "operator": "=", "value": ruleID}); err != nil {
			return err
		}
		spans := make([]map[string]any, 0)
		for _, r := range schedule.Rules {
			if r.Weekday < 0 || r.Weekday > 6 || r.Entrada == nil {
				continue
			}
			start := minutesSeconds(r.Entrada)
			end := minutesSeconds(r.Saida)
			if r.Saida == nil {
				end = 86399
			}
			if end > start {
				span := map[string]any{"time_zone_id": tzID, "start": start, "end": end}
				for k, v := range dayFlags(r.Weekday) {
					span[k] = v
				}
				spans = append(spans, span)
			} else if r.Saida != nil {
				span := map[string]any{"time_zone_id": tzID, "start": start, "end": 86399}
				for k, v := range dayFlags(r.Weekday) {
					span[k] = v
				}
				spans = append(spans, span)
				next := (r.Weekday + 1) % 7
				span = map[string]any{"time_zone_id": tzID, "start": 0, "end": end}
				for k, v := range dayFlags(next) {
					span[k] = v
				}
				spans = append(spans, span)
			}
		}
		if err := c.createOrModifyObjects(ctx, "time_spans", spans); err != nil {
			return err
		}
		if err := c.createOrModifyObjects(ctx, "access_rule_time_zones", []map[string]any{{"access_rule_id": ruleID, "time_zone_id": tzID}}); err != nil {
			return err
		}
		for _, registration := range schedule.EmployeeRegistrations {
			uid := byReg[strings.TrimSpace(registration)]
			if uid == 0 {
				continue
			}
			if err := c.createOrModifyObjects(ctx, "user_access_rules", []map[string]any{{"user_id": uid, "access_rule_id": ruleID}}); err != nil {
				return err
			}
		}
		// O iDFace exige que a regra esteja associada ao portal para efetivar a
		// liberação. Se o firmware não expõe portais, as demais etapas continuam
		// úteis para o modo de contingência.
		if err := c.destroyObjects(ctx, "portal_access_rules", map[string]any{"object": "portal_access_rules", "field": "access_rule_id", "operator": "=", "value": ruleID}); err != nil {
			return err
		}
		for _, portal := range portals {
			pid := flexRaw(portal["id"])
			if pid == "" {
				continue
			}
			portalID, parseErr := strconv.ParseInt(pid, 10, 64)
			if parseErr != nil || portalID <= 0 {
				continue
			}
			if err := c.createOrModifyObjects(ctx, "portal_access_rules", []map[string]any{{"portal_id": portalID, "access_rule_id": ruleID}}); err != nil {
				return err
			}
		}
	}
	return nil
}

func findNamedID(rows []map[string]json.RawMessage, prefix string) int64 {
	for _, row := range rows {
		name := flexRaw(row["name"])
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		id, err := strconv.ParseInt(flexRaw(row["id"]), 10, 64)
		if err == nil && id > 0 {
			return id
		}
	}
	return 0
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
		// O iDFace exige application/octet-stream neste endpoint. Embora o
		// conteúdo seja JPEG, enviar image/jpeg faz alguns firmwares tentarem
		// interpretar o corpo como texto/hex e retornarem erros como
		// "invalid hexadecimal digit" durante a sincronização.
		data, status, err := c.postBytes(ctx, endpoint, "application/octet-stream", u.Image)
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
