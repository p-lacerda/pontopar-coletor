// Reprocessa somente as batidas de usuários explicitamente vinculados no Thera.
// Não altera cursor.json nem os cadastros/faces do iDFace. Deve rodar no PC da LAN.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/p-lacerda/pontopar-coletor/internal/config"
	"github.com/p-lacerda/pontopar-coletor/internal/idface"
	"github.com/p-lacerda/pontopar-coletor/internal/thera"
)

type logger struct{}

func (logger) Infof(format string, args ...any)  { log.Printf(format, args...) }
func (logger) Errorf(format string, args ...any) { log.Printf("ERRO: "+format, args...) }

func main() {
	configDir := flag.String("config-dir", filepath.Join(os.Getenv("ProgramData"), "PontoParColetor"), "pasta do config.json do coletor")
	flag.Parse()
	if err := run(*configDir); err != nil {
		log.Fatal(err)
	}
}

func run(dir string) error {
	cfg, err := config.Load(dir)
	if err != nil {
		return err
	}
	if cfg.DeviceIdInt() != 4409419584542394 {
		return fmt.Errorf("deviceId não é o Show de Bola; nenhum dado enviado")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	device := idface.New(cfg.DeviceBase(), cfg.Login, cfg.Password, &http.Client{Timeout: 20 * time.Second}, logger{})
	api := thera.New(cfg.TheraDaoURL(), &http.Client{Timeout: 30 * time.Second})
	if err := device.EnsureSession(ctx); err != nil {
		return fmt.Errorf("login no Control iD: %w", err)
	}
	var cursor int64
	var seen, sent int
	for {
		logs, err := device.LoadNewAccessLogs(ctx, cursor)
		if err != nil {
			return fmt.Errorf("ler access_logs após %d: %w", cursor, err)
		}
		if len(logs) == 0 {
			break
		}
		var next = cursor
		for _, item := range logs {
			id, err := item.IdNum()
			if err != nil {
				return fmt.Errorf("access_log com ID inválido após %d: %w", cursor, err)
			}
			if id > next {
				next = id
			}
			seen++
			user := strings.TrimSpace(string(item.UserId))
			if user != "900000004" && user != "900000006" {
				continue
			}
			deviceID := cfg.DeviceIdInt()
			if parsed, err := item.DeviceIdNum(); err == nil {
				deviceID = parsed
			}
			if deviceID != cfg.DeviceIdInt() {
				return fmt.Errorf("access_log %d pertence a outro device", id)
			}
			logType := string(item.LogTypeId)
			if logType == "" {
				logType = "-1"
			}
			payload := thera.DaoPayload{DeviceId: deviceID, Origem: "coletor-replay", ObjectChanges: []thera.ObjectChange{{
				Object: "access_logs", Type: "inserted", Values: thera.DaoValues{
					Id: strconv.FormatInt(id, 10), Time: string(item.Time), Event: string(item.Event),
					DeviceId: strconv.FormatInt(deviceID, 10), UserId: user,
					PortalId: string(item.PortalId), LogTypeId: logType, Confidence: string(item.Confidence),
				},
			}}}
			if err := api.PostDao(ctx, payload); err != nil {
				return fmt.Errorf("enviar access_log %d: %w", id, err)
			}
			sent++
			log.Printf("reenviado id=%d user=%s time=%s", id, user, item.Time)
			// /dao confirma recebimento antes de gravar o ponto. Espaça os envios
			// para preservar a ordem por pessoa e não sobrecarregar a API.
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Second):
			}
		}
		if next <= cursor {
			return fmt.Errorf("leitura sem progresso após %d", cursor)
		}
		cursor = next // cursor LOCAL desta execução; nunca toca em cursor.json.
	}
	log.Printf("concluído: %d access_logs examinados, %d batidas dos dois usuários reenviadas; cursor.json intacto", seen, sent)
	return nil
}
