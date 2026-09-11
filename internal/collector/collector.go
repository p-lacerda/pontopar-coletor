// Package collector orquestra o loop de coleta: fala com o iDFace, traduz
// user_id -> matrícula, encaminha ao Thera e avança o cursor — só depois de
// cada 200 do Thera (garantia de NÃO-PERDA).
//
// Este é um porte fiel do collector.mjs (Node) com melhorias obrigatórias:
// timeouts HTTP separados (device LAN vs Thera internet), cursor atômico,
// diretório-base = os.Executable(), parse defensivo de id, e ctx em tudo para
// shutdown limpo quando o SCM do Windows mandar parar.
package collector

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/p-lacerda/pontopar-coletor/internal/config"
	"github.com/p-lacerda/pontopar-coletor/internal/idface"
	"github.com/p-lacerda/pontopar-coletor/internal/store"
	"github.com/p-lacerda/pontopar-coletor/internal/thera"
)

// itoa converte int64 para string (helper local).
func itoa(n int64) string { return strconv.FormatInt(n, 10) }

// Logger é a interface mínima de log usada pelo coletor.
type Logger interface {
	Infof(format string, args ...any)
	Errorf(format string, args ...any)
}

// Timeouts HTTP (o Node não tinha nenhum; em Go SEMPRE setar para não travar
// para sempre se a conexão pendurar).
const (
	deviceHTTPTimeout = 15 * time.Second // LAN: curto
	theraHTTPTimeout  = 30 * time.Second // internet: um pouco maior
)

// UpdateHook abstrai o auto-update, para o loop poder checar novas versões
// periodicamente sem acoplar o coletor ao pacote updater. É opcional (nil
// desliga a checagem). O coletor só chama CheckAndApply em pontos SEGUROS do
// ciclo (entre ticks, sem batida em voo) para não interromper um forward.
type UpdateHook interface {
	// CheckAndApply verifica e aplica update. Se updated=true, o exe no disco
	// foi trocado e o processo precisa reiniciar (o coletor chamará
	// OnUpdated e encerrará o loop).
	CheckAndApply(ctx context.Context) (updated bool, newVersion string, err error)
}

// OnUpdatedFunc é chamado depois que um update foi aplicado no disco e o loop
// já parou de coletar. Para serviço: encerra o processo com código !=0 (SCM
// reinicia). Para modo interativo: apenas loga pedindo reinício manual.
type OnUpdatedFunc func(newVersion string)

// Collector é o coletor completo.
type Collector struct {
	cfg    *config.Config
	dir    string
	log    Logger
	device *idface.Client
	thera  *thera.Client
	cursor *store.Cursor

	updater        UpdateHook
	updateEvery    time.Duration
	onUpdated      OnUpdatedFunc
	lastSync       time.Time
	syncEvery      time.Duration
	lastClockSync  time.Time
	clockSyncEvery time.Duration
}

// New monta um coletor a partir da config e do diretório-base (onde ficam
// config.json/cursor.json/log).
func New(cfg *config.Config, dir string, log Logger) *Collector {
	deviceHTTP := &http.Client{Timeout: deviceHTTPTimeout}
	theraHTTP := &http.Client{Timeout: theraHTTPTimeout}

	return &Collector{
		cfg:       cfg,
		dir:       dir,
		log:       log,
		device:    idface.New(cfg.DeviceBase(), cfg.Login, cfg.Password, deviceHTTP, log),
		thera:     thera.New(cfg.TheraDaoURL(), theraHTTP),
		cursor:    store.NewCursor(dir),
		syncEvery: 5 * time.Minute,
		// Mantém o relógio do terminal alinhado mesmo depois de uma troca de
		// horário no Windows. A chamada é pequena e best-effort.
		clockSyncEvery: 5 * time.Minute,
	}
}

// SetUpdater habilita o auto-update no loop. every é o intervalo entre
// checagens; onUpdated é chamado após aplicar (com o loop já parado).
// Passar hook nil desliga a checagem.
func (c *Collector) SetUpdater(hook UpdateHook, every time.Duration, onUpdated OnUpdatedFunc) {
	c.updater = hook
	c.updateEvery = every
	c.onUpdated = onUpdated
}

// tick executa um ciclo de coleta DRENANDO TODO O BACKLOG: garante a sessão e
// então processa lotes repetidamente enquanto houver batidas represadas.
//
// Por que em loop: LoadNewAccessLogs devolve no máximo AccessLogsBatchLimit
// batidas por chamada. Se o Thera ficou fora por horas, pode haver muito mais
// que um lote represado no aparelho — e o buffer do iDFace rotaciona (~10.000).
// Puxar só um lote por tick arriscaria perder batidas antigas antes de drená-las.
//
// O loop repete enquanto (a) o último lote veio CHEIO (== limit, sinal de que
// ainda há mais) E (b) houve PROGRESSO (o cursor avançou nesse lote). Para em:
//   - erro de forward/persistência -> propaga (Thera fora); reprocessa no
//     próximo tick, cursor no último confirmado (não-perda);
//   - lote < limit -> backlog drenado, nada mais a fazer agora;
//   - sem progresso -> guard anti-loop-infinito (lote cheio só de ids inválidos
//     ou cursor que não mexeu), evita girar para sempre.
//
// O cursor JAMAIS avança sem um 200 do Thera — a garantia de não-perda é a
// mesma de antes, apenas repetida por lote. O ctx é respeitado entre iterações
// para parada limpa do serviço.
func (c *Collector) tick(ctx context.Context) error {
	if err := c.device.EnsureSession(ctx); err != nil {
		return err
	}
	if c.lastClockSync.IsZero() || time.Since(c.lastClockSync) >= c.clockSyncEvery {
		now := time.Now().Truncate(time.Second)
		if err := c.device.SyncClock(ctx, now); err != nil {
			c.log.Errorf("sincronização do relógio falhou (batidas continuam): %v", err)
		} else {
			c.lastClockSync = now
		}
	}
	// A sincronização é best-effort: indisponibilidade temporária do endpoint
	// não pode interromper a importação fiscal das batidas.
	if c.lastSync.IsZero() || time.Since(c.lastSync) >= c.syncEvery {
		if err := c.syncUsers(ctx); err != nil {
			c.log.Errorf("sincronização de usuários/faces falhou (batidas continuam): %v", err)
		} else {
			c.lastSync = time.Now()
		}
	}

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		cursorAntes := c.cursor.Get()
		lote, err := c.drainOnce(ctx)
		if err != nil {
			// Erro de forward/persistência/leitura: propaga. Cursor está no
			// último confirmado; o próximo tick retenta a partir daí.
			return err
		}
		cursorDepois := c.cursor.Get()
		progrediu := cursorDepois > cursorAntes

		// Só continua drenando se o lote veio cheio (há mais represado) E
		// houve progresso (guard anti-loop-infinito).
		if lote < idface.AccessLogsBatchLimit || !progrediu {
			return nil
		}
	}
}

func (c *Collector) syncUsers(ctx context.Context) error {
	manifest, err := c.thera.GetSync(ctx)
	if err != nil {
		return err
	}
	if manifest.DeviceID != "" && c.cfg.DeviceIdInt() != 0 && manifest.DeviceID != itoa(c.cfg.DeviceIdInt()) {
		return fmt.Errorf("manifesto é do device %s, configurado %s", manifest.DeviceID, itoa(c.cfg.DeviceIdInt()))
	}
	var face *thera.DeviceConfiguration
	if manifest.Configuration != nil {
		config := *manifest.Configuration
		// Valores preenchidos no Setup têm precedência local; a API continua
		// sendo o canal de sincronização quando o coletor volta online.
		if c.cfg.Facial.IdentificationDistanceCm != 0 {
			config.IdentificationDistanceCm = c.cfg.Facial.IdentificationDistanceCm
			config.EnablePhotoUpload = c.cfg.Facial.EnablePhotoUpload
			config.LivenessMode = c.cfg.Facial.LivenessMode
			config.LimitDisplayRegion = c.cfg.Facial.LimitDisplayRegion
		}
		// O campo local permite escolher o modo mesmo com uma API antiga. Se
		// estiver vazio (config legado), usa o manifesto; sem ambos, attendance.
		mode := strings.ToLower(strings.TrimSpace(c.cfg.Facial.TerminalMode))
		if mode == "" {
			mode = strings.ToLower(strings.TrimSpace(config.AttendanceMode))
		}
		if mode == "" {
			mode = "attendance"
		}
		config.AttendanceMode = mode
		face = &config
	}
	// Lê o aparelho ANTES de aplicar o manifesto. O servidor compara esta
	// observação com a última registrada e reconhece mudanças feitas diretamente
	// no Control iD (nome/ativação) sem confundi-las com o primeiro sync.
	before, err := c.device.ListUsers(ctx)
	if err != nil {
		return err
	}
	if err := c.thera.PostSyncResult(ctx, syncObservations(before)); err != nil {
		return err
	}
	// Uma alteração observada no aparelho pode ter atualizado o Thera; recarrega
	// o manifesto para aplicar o estado resultante no mesmo ciclo.
	manifest, err = c.thera.GetSync(ctx)
	if err != nil {
		return err
	}
	users := make([]idface.User, 0, len(manifest.Users))
	for _, wanted := range manifest.Users {
		image, err := c.thera.GetBinary(ctx, wanted.FaceURL)
		if err != nil {
			return fmt.Errorf("face matrícula %s: %w", wanted.Registration, err)
		}
		users = append(users, idface.User{Registration: wanted.Registration, Name: wanted.Name, Enabled: wanted.Enabled, Image: image})
	}
	if err := c.device.UpsertUsers(ctx, users, time.Now().Unix()); err != nil {
		return err
	}
	if face != nil && face.AttendanceMode == "access" && face.EnforceSchedules {
		schedules := make([]idface.Schedule, 0, len(manifest.Configuration.Schedules))
		for _, s := range manifest.Configuration.Schedules {
			rules := make([]idface.ScheduleRule, 0, len(s.Rules))
			for _, r := range s.Rules {
				rules = append(rules, idface.ScheduleRule{Weekday: r.Weekday, Entrada: r.Entrada, SaidaIntervalo: r.SaidaIntervalo, RetornoIntervalo: r.RetornoIntervalo, Saida: r.Saida})
			}
			schedules = append(schedules, idface.Schedule{ScheduleID: s.ScheduleID, Name: s.Name, EmployeeRegistrations: s.EmployeeRegistrations, Rules: rules})
		}
		if err := c.device.ApplySchedules(ctx, schedules); err != nil {
			return fmt.Errorf("aplicar escalas: %w", err)
		}
		c.log.Infof("escalas sincronizadas no Control iD: %d", len(schedules))
	}
	// Só muda o terminal para standalone DEPOIS que todas as regras foram
	// publicadas. Assim uma falha intermediária nunca deixa o relógio em modo
	// de autorização sem uma regra válida.
	if face != nil {
		if err := c.device.SetFacialConfiguration(ctx, face.EnablePhotoUpload, face.LivenessMode, face.LimitDisplayRegion, face.IdentificationDistanceCm, face.AttendanceMode); err != nil {
			return fmt.Errorf("aplicar configuração facial: %w", err)
		}
	}
	observed, err := c.device.ListUsers(ctx)
	if err != nil {
		return err
	}
	result := make([]thera.SyncObservation, 0, len(observed))
	importar := make(map[string]bool, len(manifest.Users))
	for _, wanted := range manifest.Users {
		importar[wanted.Registration] = wanted.ImportFace
	}
	for _, user := range observed {
		if user.ImageRegistered && importar[user.Registration] {
			image, faceErr := c.device.GetUserImage(ctx, user.Id)
			if faceErr != nil {
				return fmt.Errorf("ler face matrícula %s: %w", user.Registration, faceErr)
			}
			if faceErr = c.thera.PostFace(ctx, user.Registration, image); faceErr != nil {
				return fmt.Errorf("enviar face matrícula %s: %w", user.Registration, faceErr)
			}
		}
		result = append(result, thera.SyncObservation{UserID: itoa(user.Id), Registration: user.Registration, Name: user.Name, Enabled: user.Enabled, FaceEnrolled: user.ImageRegistered})
	}
	if err := c.thera.PostSyncResult(ctx, result); err != nil {
		return err
	}
	c.log.Infof("sincronização concluída: %d usuários observados", len(result))
	return nil
}

func syncObservations(users []idface.UserSnapshot) []thera.SyncObservation {
	result := make([]thera.SyncObservation, 0, len(users))
	for _, user := range users {
		result = append(result, thera.SyncObservation{UserID: itoa(user.Id), Registration: user.Registration, Name: user.Name, Enabled: user.Enabled, FaceEnrolled: user.ImageRegistered})
	}
	return result
}

// drainOnce processa UM lote: busca as próximas batidas (id > cursor), traduz
// user_id -> matrícula e encaminha cada uma ao Thera, avançando o cursor após
// cada 200. Devolve o tamanho do lote lido (para o tick decidir se drena mais)
// e o erro que aborta a drenagem.
//
// Se PostDao/cursor.Set falharem, o erro é propagado IMEDIATAMENTE (aborta o
// restante do laço): o cursor não avança nas batidas seguintes, reprocessadas
// no próximo tick. Este é o coração da não-perda quando o Thera está fora.
func (c *Collector) drainOnce(ctx context.Context) (int, error) {
	cursor := c.cursor.Get()
	logs, err := c.device.LoadNewAccessLogs(ctx, cursor)
	if err != nil {
		return 0, err
	}
	if len(logs) == 0 {
		return 0, nil
	}

	// Ordena ascendente por id numérico (o device já manda order:["id"], mas
	// reordenamos por garantia, como o Node). Batidas com id inválido vão para
	// o fim e são puladas no laço.
	sort.SliceStable(logs, func(i, j int) bool {
		a, ea := logs[i].IdNum()
		b, eb := logs[j].IdNum()
		if ea != nil || eb != nil {
			// inválidos por último
			return ea == nil && eb != nil
		}
		return a < b
	})

	// Traduz em lote os user_id -> registration (uma consulta cobre o lote).
	userIds := make([]string, 0, len(logs))
	for _, l := range logs {
		userIds = append(userIds, string(l.UserId))
	}
	c.device.ResolveRegistrations(ctx, userIds)

	for _, l := range logs {
		select {
		case <-ctx.Done():
			return len(logs), ctx.Err()
		default:
		}

		id, err := l.IdNum()
		if err != nil {
			// id inválido: não dá para avançar o cursor com segurança; loga e
			// pula (o Node corromperia o cursor com NaN).
			c.log.Errorf("batida com id invalido ignorada: id=%q user=%q time=%q: %v",
				l.Id, l.UserId, l.Time, err)
			continue
		}

		payload := c.buildPayload(l)
		if err := c.thera.PostDao(ctx, payload); err != nil {
			// Thera fora / não-2xx: aborta ANTES do setCursor. Cursor fica no
			// último confirmado; reprocessa no próximo tick.
			return len(logs), err
		}

		if err := c.cursor.Set(id); err != nil {
			// Falha ao persistir cursor: aborta para não avançar em memória sem
			// persistir. Reprocessa (Thera deduplica) — não perde nada.
			return len(logs), err
		}

		reg := c.device.Registration(string(l.UserId))
		if reg == "" {
			reg = "-"
		}
		c.log.Infof("batida encaminhada: id=%s user=%s matricula=%s time=%s",
			l.Id, l.UserId, reg, l.Time)
	}
	return len(logs), nil
}

// buildPayload monta o DaoPayload de uma batida (regras de fallback do Node).
func (c *Collector) buildPayload(l idface.AccessLog) thera.DaoPayload {
	// device_id (dentro de values, string): l.device_id ou cfg.deviceId ou "".
	valDeviceId := string(l.DeviceId)
	if valDeviceId == "" && c.cfg.DeviceIdInt() != 0 {
		valDeviceId = itoa(c.cfg.DeviceIdInt())
	}

	logTypeId := string(l.LogTypeId)
	if logTypeId == "" {
		logTypeId = "-1" // default do Node (l.log_type_id ?? "-1")
	}

	values := thera.DaoValues{
		Id:         string(l.Id),
		Time:       string(l.Time),
		Event:      string(l.Event),
		DeviceId:   valDeviceId,
		UserId:     string(l.UserId),
		PortalId:   string(l.PortalId),
		LogTypeId:  logTypeId,
		Confidence: string(l.Confidence),
	}
	// registration só se houver (nunca string vazia; omitempty cobre).
	if reg := c.device.Registration(string(l.UserId)); reg != "" {
		values.Registration = reg
	}

	// Envelope device_id (int): l.device_id ou cfg.deviceId ou 0.
	envDeviceId := c.cfg.DeviceIdInt()
	if n, err := l.DeviceIdNum(); err == nil {
		envDeviceId = n
	}

	return thera.DaoPayload{
		ObjectChanges: []thera.ObjectChange{{
			Object: "access_logs",
			Type:   "inserted",
			Values: values,
		}},
		DeviceId: envDeviceId,
		Origem:   "coletor-local",
	}
}

// Run roda o loop principal até o contexto ser cancelado (shutdown limpo do
// serviço). Entre ticks, dorme pollSeconds respeitando ctx.Done(). Se um
// UpdateHook estiver configurado, checa novas versões nos intervalos
// definidos, sempre num ponto seguro do ciclo (fora de um forward em voo).
func (c *Collector) Run(ctx context.Context) {
	c.log.Infof("Coletor PontoPar iniciado · aparelho=%s · thera=%s · poll=%ds",
		c.cfg.DeviceBase(), c.cfg.TheraBase, c.cfg.Poll())

	poll := time.Duration(c.cfg.Poll()) * time.Second
	nextUpdateCheck := time.Time{}
	if c.updater != nil && c.updateEvery > 0 {
		// Primeira checagem alguns minutos após o arranque (não logo no boot,
		// para não competir com a rede subindo).
		nextUpdateCheck = time.Now().Add(2 * time.Minute)
	}

	for {
		if err := c.tick(ctx); err != nil {
			if ctx.Err() != nil {
				// Cancelado: sai sem barulho.
				return
			}
			c.log.Errorf("erro no ciclo (tenta de novo): %v", err)
			// Força relogin no próximo tick (igual session=null do Node).
			c.device.ClearSession()
		}

		// Ponto seguro: sem batida em voo. Checa update se estiver na hora.
		if c.updater != nil && c.updateEvery > 0 && !nextUpdateCheck.IsZero() && time.Now().After(nextUpdateCheck) {
			nextUpdateCheck = time.Now().Add(c.updateEvery)
			if c.checkUpdate(ctx) {
				// Update aplicado: o loop encerra e o onUpdated decide o restart.
				return
			}
		}

		select {
		case <-ctx.Done():
			c.log.Infof("coletor finalizando (contexto cancelado)")
			return
		case <-time.After(poll):
		}
	}
}

// checkUpdate roda a checagem de auto-update (best-effort). Devolve true se um
// update foi aplicado (o chamador deve encerrar o loop). Nunca derruba o
// coletor por falha de update — só loga.
func (c *Collector) checkUpdate(ctx context.Context) bool {
	updated, newVersion, err := c.updater.CheckAndApply(ctx)
	if err != nil {
		c.log.Errorf("auto-update falhou (seguindo na versao atual): %v", err)
		return false
	}
	if !updated {
		return false
	}
	c.log.Infof("auto-update: nova versao %s aplicada no disco", newVersion)
	if c.onUpdated != nil {
		c.onUpdated(newVersion)
	}
	return true
}
