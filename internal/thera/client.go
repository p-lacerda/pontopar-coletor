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
	daoURL string
	http   *http.Client
}

// New cria um cliente Thera. daoURL é a URL completa do endpoint /dao.
// httpClient deve ter timeout (internet).
func New(daoURL string, httpClient *http.Client) *Client {
	return &Client{daoURL: daoURL, http: httpClient}
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
