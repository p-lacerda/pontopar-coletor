// Package thera encaminha as batidas ao webhook /dao do Thera (nuvem, HTTPS).
//
// O contrato do payload segue o webhook "Monitor /dao" do Control iD: os
// valores numéricos vão como STRING dentro de "values", mas o device_id do
// envelope externo vai como INT. Só uma resposta 2xx autoriza o coletor a
// avançar o cursor — é o coração da não-perda.
package thera

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// DaoValues são os campos da batida (todos STRING, por contrato).
type DaoValues struct {
	Id           string `json:"id"`
	Time         string `json:"time"`
	Event        string `json:"event"`
	DeviceId     string `json:"device_id"`
	UserId       string `json:"user_id"`
	PortalId     string `json:"portal_id"`
	LogTypeId    string `json:"log_type_id"`
	Confidence   string `json:"confidence"`
	Registration string `json:"registration,omitempty"` // OMITIR quando vazio
}

// ObjectChange é um item de object_changes.
type ObjectChange struct {
	Object string    `json:"object"` // "access_logs"
	Type   string    `json:"type"`   // "inserted"
	Values DaoValues `json:"values"`
}

// DaoPayload é o corpo do POST /dao.
type DaoPayload struct {
	ObjectChanges []ObjectChange `json:"object_changes"`
	DeviceId      int64          `json:"device_id"` // INT no envelope externo
	Origem        string         `json:"_origem"`   // "coletor-local"
}

// Client encaminha payloads ao Thera.
type Client struct {
	daoURL  string
	syncURL string
	http    *http.Client
}

// New cria um cliente Thera. daoURL é a URL completa do endpoint /dao.
// httpClient deve ter timeout (internet).
func New(daoURL string, httpClient *http.Client) *Client {
	return &Client{daoURL: daoURL, syncURL: strings.TrimSuffix(daoURL, "/dao") + "/sync", http: httpClient}
}

// SyncUser é o cadastro desejado pelo Thera. FaceURL é temporária e só é
// baixada em memória pelo coletor; nunca é persistida no PC.
type SyncUser struct {
	EmployeeID   string `json:"employeeId"`
	Name         string `json:"name"`
	Registration string `json:"registration"`
	Enabled      bool   `json:"enabled"`
	FaceURL      string `json:"faceUrl"`
	ImportFace   bool   `json:"importFace"`
}

type SyncManifest struct {
	DeviceID string     `json:"deviceId"`
	Users    []SyncUser `json:"users"`
	// Configuração pendente/desired do terminal. Pequena e idempotente: não é
	// fila em memória; o coletor lê do servidor a cada sincronização.
	Configuration *DeviceConfiguration `json:"configuration,omitempty"`
}
type DeviceConfiguration struct {
	EnablePhotoUpload        bool    `json:"enablePhotoUpload"`
	LivenessMode             bool    `json:"livenessMode"`
	LimitDisplayRegion       bool    `json:"limitDisplayRegion"`
	IdentificationDistanceCm float64 `json:"identificationDistanceCm,omitempty"`
	// Attendance é o padrão para marcação de ponto; access habilita as regras
	// de acesso/horário do terminal quando explicitamente escolhido.
	AttendanceMode   string           `json:"attendanceMode,omitempty"`
	EnforceSchedules bool             `json:"enforceSchedules"`
	Schedules        []DeviceSchedule `json:"schedules,omitempty"`
}

type DeviceScheduleRule struct {
	Weekday          int  `json:"weekday"`
	Entrada          *int `json:"entrada"`
	SaidaIntervalo   *int `json:"saidaIntervalo"`
	RetornoIntervalo *int `json:"retornoIntervalo"`
	Saida            *int `json:"saida"`
}

type DeviceSchedule struct {
	ScheduleID            string               `json:"scheduleId"`
	Name                  string               `json:"name"`
	EmployeeRegistrations []string             `json:"employeeRegistrations"`
	Rules                 []DeviceScheduleRule `json:"rules"`
}
type SyncObservation struct {
	UserID       string `json:"userId"`
	Registration string `json:"registration"`
	Name         string `json:"name"`
	Enabled      bool   `json:"enabled"`
	FaceEnrolled bool   `json:"faceEnrolled"`
}

// GetSync busca o manifesto autenticado pelo segredo embutido na URL do dao.
func (c *Client) GetSync(ctx context.Context) (SyncManifest, error) {
	var out SyncManifest
	data, status, err := c.request(ctx, http.MethodGet, c.syncURL, nil, "")
	if err != nil {
		return out, err
	}
	if status < 200 || status >= 300 {
		return out, fmt.Errorf("Thera /sync respondeu %d: %s", status, strings.TrimSpace(string(data)))
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return out, fmt.Errorf("manifesto /sync inválido: %w", err)
	}
	return out, nil
}

func (c *Client) PostSyncResult(ctx context.Context, users []SyncObservation) error {
	body, err := json.Marshal(map[string]any{"users": users})
	if err != nil {
		return err
	}
	data, status, err := c.request(ctx, http.MethodPost, c.syncURL+"/result", bytes.NewReader(body), "application/json")
	if err != nil {
		return err
	}
	if status < 200 || status >= 300 {
		return fmt.Errorf("Thera /sync/result respondeu %d: %s", status, strings.TrimSpace(string(data)))
	}
	return nil
}

func (c *Client) PostFace(ctx context.Context, registration string, image []byte) error {
	body, err := json.Marshal(map[string]string{"registration": registration, "imageBase64": base64.StdEncoding.EncodeToString(image)})
	if err != nil {
		return err
	}
	data, status, err := c.request(ctx, http.MethodPost, c.syncURL+"/face", bytes.NewReader(body), "application/json")
	if err != nil {
		return err
	}
	if status < 200 || status >= 300 {
		return fmt.Errorf("Thera /sync/face respondeu %d: %s", status, strings.TrimSpace(string(data)))
	}
	return nil
}

// GetBinary baixa uma face temporária retornada pelo manifesto. O limite evita
// armazenar acidentalmente arquivos grandes enviados por uma URL comprometida.
func (c *Client) GetBinary(ctx context.Context, target string) ([]byte, error) {
	if strings.TrimSpace(target) == "" {
		return nil, nil
	}
	data, status, err := c.request(ctx, http.MethodGet, target, nil, "")
	if err != nil {
		return nil, err
	}
	if status < 200 || status >= 300 {
		return nil, fmt.Errorf("baixar face respondeu %d", status)
	}
	if len(data) > 2<<20 {
		return nil, fmt.Errorf("face excede 2 MB")
	}
	return data, nil
}

func (c *Client) request(ctx context.Context, method, target string, body io.Reader, contentType string) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return nil, 0, err
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20+1))
	return data, resp.StatusCode, err
}

// PostDao envia o payload ao Thera. Erro se a resposta não for 2xx.
func (c *Client) PostDao(ctx context.Context, payload DaoPayload) error {
	buf, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.daoURL, bytes.NewReader(buf))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	// Drena o corpo (permite reuso de conexão keep-alive) e captura para o erro.
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		msg := strings.TrimSpace(string(body))
		if msg != "" {
			return fmt.Errorf("Thera respondeu %d: %s", resp.StatusCode, msg)
		}
		return fmt.Errorf("Thera respondeu %d", resp.StatusCode)
	}
	return nil
}
