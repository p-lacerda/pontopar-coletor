// Package collector orquestra o loop de coleta: fala com o iDFace, traduz
// user_id -> matrícula, encaminha ao Thera e avança o cursor — só depois de
// cada 200 do Thera (garantia de NÃO-PERDA).
//
// Este é um porte fiel do collector.mjs (Node) com melhorias obrigatórias:
// timeouts HTTP separados (device LAN vs Thera internet), cursor atômico,
// diretório-base = os.Executable(), parse defensivo de id, e ctx em tudo para
// shutdown limpo quando o SCM do Windows mandar parar.
//
// TRAVA ANTI-CORRUPÇÃO DE HORÁRIO (motivo da reescrita): o bug histórico foi um
// replay em massa que CARIMBOU dezenas de pessoas no MESMO segundo porque as
// batidas perderam o `time` real durante o reenvio em lote. O backend deriva o
// `punchedAt` E a chave de dedupe do campo `time`; uma batida sem `time` vira um
// carimbo "agora" e colapsa vários eventos num segundo só. Por isso o coletor
// AGORA RECUSA (nunca envia) qualquer batida sem `time` válido e AUDITA cada run.
package collector

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/p-lacerda/pontopar-coletor/internal/audit"
	"github.com/p-lacerda/pontopar-coletor/internal/config"
	"github.com/p-lacerda/pontopar-coletor/internal/idface"
	"github.com/p-lacerda/pontopar-coletor/internal/state"
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
	maxRetryBackoff   = 30 * time.Second // teto do backoff entre retentativas
)

// fullSyncCursorFile é o cursor SEPARADO que torna o full-sync retomável sem
// mexer no cursor.json da coleta incremental.
const fullSyncCursorFile = "fullsync.json"

// fullSyncPeriodFile guarda o período (from/to) do refill em andamento, para o
// "Retomar" continuar com o MESMO recorte em vez de virar um refill total.
const fullSyncPeriodFile = "fullsync-period.json"

// refillShutdownWait é quanto o coletor espera, no desligamento, um refill em
// andamento encerrar e CONFIRMAR ao Thera (ack "failed: interrompido"). Cabe no
// WaitHint que o serviço renova durante o StopPending.
const refillShutdownWait = 15 * time.Second

// validUnixTime valida o `time` de um access_log. Rejeita vazio, "0", negativo e
// não-numérico — exatamente os casos que fariam o backend carimbar "agora" e
// colapsar horários. Um epoch em segundos ou milissegundos (ambos > 0) passa; o
// backend já distingue segundos de ms.
func validUnixTime(s string) (int64, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, false
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n <= 0 {
		return 0, false
	}
	return n, true
}

// inPeriod diz se um `time` de access_log (epoch em segundos OU milissegundos)
// cai no intervalo [fromUnix, toUnix] em SEGUNDOS. from/to <= 0 significam
// "sem limite" naquele lado; os dois <= 0 => sempre no período (full total).
func inPeriod(n, fromUnix, toUnix int64) bool {
	if fromUnix <= 0 && toUnix <= 0 {
		return true
	}
	ts := n
	if ts > 1_000_000_000_000 { // > ~2001 em ms → é milissegundos
		ts /= 1000
	}
	if fromUnix > 0 && ts < fromUnix {
		return false
	}
	if toUnix > 0 && ts > toUnix {
		return false
	}
	return true
}

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

	st *state.State // estado vivo para a UI (pode ser nil em testes simples)
	au *audit.Log   // auditoria persistente (pode ser nil em testes simples)

	batchSize    int
	forwardBatch int // nº máx. de batidas por POST ao Thera (envelope object_changes)
	maxRetries   int
	retryBase    time.Duration
	overrides    map[string]string // user_id -> matrícula (prioridade sobre o device)

	fullCursor      *store.Cursor
	fullSyncRunning atomic.Bool

	updater          UpdateHook
	updateEvery      time.Duration
	onUpdated        OnUpdatedFunc
	lastSync         time.Time
	syncEvery        time.Duration
	lastClockSync    time.Time
	clockSyncEvery   time.Duration
	lastCommandCheck time.Time
	commandEvery     time.Duration

	// wake acorda o loop antes do fim do pollSeconds ("Coletar agora").
	wake chan struct{}

	// refills rastreia os refills em goroutine: no desligamento o coletor espera
	// eles encerrarem e confirmarem ao Thera, senão o comando ficava RUNNING.
	refills      sync.WaitGroup
	ctxMu        sync.Mutex
	runCtx       context.Context
	refillCancel context.CancelFunc
}

// New monta um coletor a partir da config e do diretório-base (onde ficam
// config.json/cursor.json/log). st e au são opcionais (nil desliga estado/auditoria).
func New(cfg *config.Config, dir string, log Logger, st *state.State, au *audit.Log) *Collector {
	deviceHTTP := &http.Client{Timeout: deviceHTTPTimeout}
	theraHTTP := &http.Client{Timeout: theraHTTPTimeout}

	c := &Collector{
		cfg:          cfg,
		dir:          dir,
		log:          log,
		device:       idface.New(cfg.DeviceBase(), cfg.Login, cfg.Password, deviceHTTP, log),
		thera:        thera.New(cfg.TheraDaoURL(), theraHTTP),
		cursor:       store.NewCursor(dir),
		fullCursor:   store.NewNamedCursor(dir, fullSyncCursorFile),
		st:           st,
		au:           au,
		batchSize:    cfg.BatchSizeOr(),
		forwardBatch: cfg.ForwardBatchOr(),
		maxRetries:   cfg.MaxRetriesOr(),
		retryBase:    cfg.RetryBackoffBase(),
		overrides:    cfg.RegistrationOverrides,
		syncEvery:    5 * time.Minute,
		// Mantém o relógio do terminal alinhado mesmo depois de uma troca de
		// horário no Windows. A chamada é pequena e best-effort.
		clockSyncEvery: 5 * time.Minute,
		// Comandos on-demand (ex.: full-sync disparado pela web): checagem leve e
		// frequente para o disparo responder em segundos.
		commandEvery: 15 * time.Second,
		wake:         make(chan struct{}, 1),
	}
	if st != nil {
		st.SetEndpoints(cfg.DeviceBase(), cfg.TheraBase)
		st.SetPoll(cfg.Poll())
		st.SetCursor(c.cursor.Get())
	}
	return c
}

// SetUpdater habilita o auto-update no loop. every é o intervalo entre
// checagens; onUpdated é chamado após aplicar (com o loop já parado).
// Passar hook nil desliga a checagem.
func (c *Collector) SetUpdater(hook UpdateHook, every time.Duration, onUpdated OnUpdatedFunc) {
	c.updater = hook
	c.updateEvery = every
	c.onUpdated = onUpdated
}

// registrationFor devolve a matrícula de um user_id. Um override local (config)
// tem PRIORIDADE sobre o de-para lido do aparelho — assim o operador conserta um
// usuário sem `registration` correto sem precisar mexer no cadastro do device.
func (c *Collector) registrationFor(userId string) string {
	if c.overrides != nil {
		if reg, ok := c.overrides[strings.TrimSpace(userId)]; ok {
			reg = strings.TrimSpace(reg)
			if reg != "" {
				return reg
			}
		}
	}
	return c.device.Registration(userId)
}

// deviceOK/deviceFail/theraOK/theraFail atualizam o estado (no-op se st==nil).
func (c *Collector) deviceOK() {
	if c.st != nil {
		c.st.DeviceOK()
	}
}
func (c *Collector) deviceFail(err error) {
	if c.st != nil {
		c.st.DeviceFail(err.Error())
	}
}
func (c *Collector) theraOK() {
	if c.st != nil {
		c.st.TheraOK()
	}
}
func (c *Collector) theraFail(err error) {
	if c.st != nil {
		c.st.TheraFail(err.Error())
	}
}

// writeAudit anexa um record (no-op se au==nil).
func (c *Collector) writeAudit(r audit.Record) {
	if c.au == nil {
		return
	}
	if err := c.au.Append(r); err != nil {
		c.log.Errorf("falha ao gravar auditoria: %v", err)
	}
}

// drainResult acumula o que um ciclo de drenagem leu/enviou/pulou, para a
// auditoria e o estado.
type drainResult struct {
	read          int
	forwarded     int
	skippedNoTime int
	skippedBadID  int
	firstID       int64
	lastID        int64
}

func (r *drainResult) add(o drainResult) {
	r.read += o.read
	r.forwarded += o.forwarded
	r.skippedNoTime += o.skippedNoTime
	r.skippedBadID += o.skippedBadID
	if o.firstID != 0 && (r.firstID == 0 || o.firstID < r.firstID) {
		r.firstID = o.firstID
	}
	if o.lastID > r.lastID {
		r.lastID = o.lastID
	}
}

// tick executa um ciclo de coleta DRENANDO TODO O BACKLOG. Ver drainOnce.
//
// Durante um full-sync on-demand o tick INTEIRO é pulado: o full-sync é o único
// que fala com o aparelho naquele momento, evitando dois remetentes concorrentes.
func (c *Collector) tick(ctx context.Context) error {
	if c.fullSyncRunning.Load() {
		return nil // full-sync em andamento tem exclusividade
	}
	if c.st != nil {
		c.st.Tick()
	}
	cursorInicial := c.cursor.Get()
	if err := c.device.EnsureSession(ctx); err != nil {
		c.deviceFail(err)
		return err
	}
	c.deviceOK()

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
			// Falha da sincronização de CADASTROS não é "Thera fora": as batidas
			// (/dao) seguem indo. Marcar o Thera offline aqui fazia o painel e a
			// bandeja dizerem "sem conexão com o Thera" com tudo funcionando.
			if c.st != nil {
				c.st.UserSyncFail(err.Error())
			}
			// Mas se quem falhou foi o THERA (rede, 5xx, segredo errado), ele está
			// fora para nós — sem isso, com o relógio ocioso, ninguém perceberia.
			var ts *theraSideError
			if errors.As(err, &ts) {
				c.theraFail(err)
			}
			c.log.Errorf("sincronização de usuários/faces falhou (batidas continuam): %v", err)
		} else {
			c.theraOK()
			c.lastSync = time.Now()
		}
	}

	var total drainResult
	var tickErr error
	for {
		select {
		case <-ctx.Done():
			tickErr = ctx.Err()
		default:
		}
		if tickErr != nil {
			break
		}

		cursorAntes := c.cursor.Get()
		res, lote, err := c.drainOnce(ctx)
		total.add(res)
		if err != nil {
			tickErr = err
			break
		}
		cursorDepois := c.cursor.Get()
		progrediu := cursorDepois > cursorAntes

		// Só continua drenando se o lote veio cheio (há mais represado) E
		// houve progresso (guard anti-loop-infinito).
		if lote < c.batchSize || !progrediu {
			break
		}
	}

	// Auditoria do tick: só registra quando houve atividade (leu/pulou algo) ou
	// erro — para não encher o audit.jsonl de ticks vazios.
	if total.read > 0 || total.skippedBadID > 0 || tickErr != nil {
		rec := audit.Record{
			Mode: audit.ModeIncremental, FromID: cursorInicial, ToID: c.cursor.Get(),
			Read: total.read, Forwarded: total.forwarded,
			SkippedNoTime: total.skippedNoTime, SkippedBadID: total.skippedBadID,
		}
		if tickErr != nil && !errors.Is(tickErr, context.Canceled) {
			rec.Err = tickErr.Error()
		}
		// Só audita se algo relevante aconteceu (evita ruído de erro de sessão sem leitura).
		if total.read > 0 || total.skippedBadID > 0 {
			c.writeAudit(rec)
		}
	}
	if c.st != nil {
		c.st.Forwarded(total.forwarded)
		c.st.SkippedNoTime(total.skippedNoTime)
	}
	return tickErr
}

func (c *Collector) syncUsers(ctx context.Context) error {
	manifest, err := c.thera.GetSync(ctx)
	if err != nil {
		return theraSide(err)
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
		return theraSide(err)
	}
	// Uma alteração observada no aparelho pode ter atualizado o Thera; recarrega
	// o manifesto para aplicar o estado resultante no mesmo ciclo.
	manifest, err = c.thera.GetSync(ctx)
	if err != nil {
		return theraSide(err)
	}
	users := make([]idface.User, 0, len(manifest.Users))
	for _, wanted := range manifest.Users {
		image, err := c.thera.GetBinary(ctx, wanted.FaceURL)
		if err != nil {
			return theraSide(fmt.Errorf("face matrícula %s: %w", wanted.Registration, err))
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
				return theraSide(fmt.Errorf("enviar face matrícula %s: %w", user.Registration, faceErr))
			}
		}
		result = append(result, thera.SyncObservation{UserID: itoa(user.Id), Registration: user.Registration, Name: user.Name, Enabled: user.Enabled, FaceEnrolled: user.ImageRegistered})
	}
	if err := c.thera.PostSyncResult(ctx, result); err != nil {
		return theraSide(err)
	}
	if c.st != nil {
		c.st.UserSync(len(result))
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

// theraSideError marca um erro vindo das chamadas ao Thera (não do relógio).
type theraSideError struct{ err error }

func (e *theraSideError) Error() string { return e.err.Error() }
func (e *theraSideError) Unwrap() error { return e.err }

func theraSide(err error) error {
	if err == nil {
		return nil
	}
	return &theraSideError{err: err}
}

// sortLogsByID ordena um lote de access_logs por id ascendente; ids inválidos
// vão para o fim.
func sortLogsByID(logs []idface.AccessLog) {
	sort.SliceStable(logs, func(i, j int) bool {
		a, ea := logs[i].IdNum()
		b, eb := logs[j].IdNum()
		if ea != nil || eb != nil {
			return ea == nil && eb != nil
		}
		return a < b
	})
}

// drainOnce processa UM lote (ver doc da struct). Devolve o drainResult, o
// tamanho do lote lido e o erro que aborta a drenagem.
func (c *Collector) drainOnce(ctx context.Context) (drainResult, int, error) {
	var res drainResult
	cursor := c.cursor.Get()
	logs, err := c.device.LoadAccessLogsPage(ctx, cursor, c.batchSize)
	if err != nil {
		c.deviceFail(err)
		return res, 0, err
	}
	c.deviceOK()
	if len(logs) == 0 {
		return res, 0, nil
	}

	sortLogsByID(logs)

	// Traduz em lote os user_id -> registration (uma consulta cobre o lote).
	userIds := make([]string, 0, len(logs))
	for _, l := range logs {
		userIds = append(userIds, string(l.UserId))
	}
	c.device.ResolveRegistrations(ctx, userIds)

	for _, l := range logs {
		select {
		case <-ctx.Done():
			return res, len(logs), ctx.Err()
		default:
		}

		res.read++
		id, err := l.IdNum()
		if err != nil {
			res.skippedBadID++
			c.log.Errorf("batida com id invalido ignorada: id=%q user=%q time=%q: %v",
				l.Id, l.UserId, l.Time, err)
			continue
		}
		if res.firstID == 0 {
			res.firstID = id
		}
		if c.st != nil {
			c.st.SeenDeviceID(id)
		}

		// TRAVA ANTI-CORRUPÇÃO: sem `time` válido, a batida NÃO é enviada (o
		// backend carimbaria "agora" e colapsaria horários). Avança o cursor para
		// não reprocessar para sempre, mas registra a anomalia.
		if _, ok := validUnixTime(string(l.Time)); !ok {
			res.skippedNoTime++
			c.log.Errorf("batida SEM time válido RECUSADA (nunca enviada p/ não corromper horário): id=%s user=%s time=%q",
				l.Id, l.UserId, l.Time)
			if err := c.cursor.Set(id); err != nil {
				return res, len(logs), err
			}
			res.lastID = id
			if c.st != nil {
				c.st.SetCursor(id)
			}
			continue
		}

		payload := c.buildPayload(l)
		if err := c.forward(ctx, payload); err != nil {
			c.theraFail(err)
			return res, len(logs), err
		}
		c.theraOK()

		if err := c.cursor.Set(id); err != nil {
			return res, len(logs), err
		}
		res.forwarded++
		res.lastID = id
		if c.st != nil {
			c.st.SetCursor(id)
		}

		reg := c.registrationFor(string(l.UserId))
		if c.st != nil {
			t, _ := validUnixTime(string(l.Time))
			c.st.RecordPunch(state.Punch{ID: id, UserID: string(l.UserId), Registration: reg, Time: t})
		}
		if reg == "" {
			reg = "-"
		}
		c.log.Infof("batida encaminhada: id=%s user=%s matricula=%s time=%s",
			l.Id, l.UserId, reg, l.Time)
	}
	return res, len(logs), nil
}

// forward envia um payload ao Thera com retentativas e backoff exponencial para
// blips transitórios. Se esgotar as tentativas, devolve o erro (o chamador aborta
// o lote e reprocessa no próximo tick — o cursor fica no último confirmado).
func (c *Collector) forward(ctx context.Context, payload thera.DaoPayload) error {
	var err error
	for attempt := 0; attempt <= c.maxRetries; attempt++ {
		if attempt > 0 {
			backoff := c.retryBase * time.Duration(int64(1)<<(attempt-1))
			if backoff > maxRetryBackoff || backoff <= 0 {
				backoff = maxRetryBackoff
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(backoff):
			}
			c.log.Infof("reenviando batida ao Thera (tentativa %d/%d) após erro: %v", attempt+1, c.maxRetries+1, err)
		}
		err = c.thera.PostDao(ctx, payload)
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
	}
	return err
}

// buildChange monta UM object_change (a batida) — regras de fallback do Node.
// Reutilizado tanto no envio unitário quanto no batelado.
func (c *Collector) buildChange(l idface.AccessLog) thera.ObjectChange {
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
	if reg := c.registrationFor(string(l.UserId)); reg != "" {
		values.Registration = reg
	}

	return thera.ObjectChange{Object: "access_logs", Type: "inserted", Values: values}
}

// envDeviceIdFor resolve o device_id do envelope (int): l.device_id ou cfg.deviceId.
func (c *Collector) envDeviceIdFor(l idface.AccessLog) int64 {
	if n, err := l.DeviceIdNum(); err == nil {
		return n
	}
	return c.cfg.DeviceIdInt()
}

// buildPayload monta o DaoPayload de UMA batida (envio unitário).
func (c *Collector) buildPayload(l idface.AccessLog) thera.DaoPayload {
	return thera.DaoPayload{
		ObjectChanges: []thera.ObjectChange{c.buildChange(l)},
		DeviceId:      c.envDeviceIdFor(l),
		Origem:        "coletor-local",
	}
}

// isRateLimited diz se o erro é um 429 do Thera e, se sim, por quanto esperar.
func isRateLimited(err error) (bool, time.Duration) {
	var he *thera.HTTPError
	if errors.As(err, &he) && he.Status == http.StatusTooManyRequests {
		return true, he.RetryAfter
	}
	return false, 0
}

// forwardBatch envia VÁRIAS batidas num único POST (envelope object_changes),
// cortando drasticamente o nº de requests (é o que evita o 429 do limitador por
// IP). Retentativa resiliente:
//   - 429 (too many requests): respeita o Retry-After (ou backoff) e tenta DE
//     NOVO indefinidamente — nunca derruba o run por rate-limit;
//   - erro transitório (rede/5xx): backoff exponencial até maxRetries, então
//     devolve o erro (o chamador para de forma retomável, cursor preservado).
//
// A API /dao é "dispara-e-esquece" (200 imediato, ingestão assíncrona e
// idempotente por device+id+time), então batelar não piora a não-perda.
func (c *Collector) sendBatch(ctx context.Context, changes []thera.ObjectChange, envDeviceId int64) error {
	if len(changes) == 0 {
		return nil
	}
	payload := thera.DaoPayload{ObjectChanges: changes, DeviceId: envDeviceId, Origem: "coletor-local"}
	transient := 0
	for {
		err := c.thera.PostDao(ctx, payload)
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if limited, retryAfter := isRateLimited(err); limited {
			wait := retryAfter
			if wait <= 0 {
				wait = c.retryBase
			}
			if wait > maxRetryBackoff {
				wait = maxRetryBackoff
			}
			c.log.Infof("Thera pediu para aguardar (429); reenviando lote de %d em %s", len(changes), wait)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(wait):
			}
			continue // 429 não conta como falha: nunca desiste
		}
		transient++
		if transient > c.maxRetries {
			return err
		}
		backoff := c.retryBase * time.Duration(int64(1)<<(transient-1))
		if backoff > maxRetryBackoff || backoff <= 0 {
			backoff = maxRetryBackoff
		}
		c.log.Infof("reenviando lote de %d ao Thera (tentativa %d/%d) após erro: %v", len(changes), transient+1, c.maxRetries+1, err)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
	}
}

// FullSyncResult resume o que um full-sync fez (para a UI e o log).
type FullSyncResult = audit.Record

// FullSync faz a varredura COMPLETA on-demand ("Consultar o Control iD completo").
// Drena o access_logs concedido do aparelho a partir de startID (use 0 para o
// histórico inteiro, ou -1 para RETOMAR um full-sync interrompido), carregando
// SEMPRE o `time` real de cada evento e nunca colapsando horários.
//
// PERÍODO: se fromUnix/toUnix (epoch em SEGUNDOS) forem > 0, só as batidas dentro
// do intervalo são enviadas (as de fora são puladas, mas o cursor avança). Zero
// em ambos = histórico total.
//
// DESEMPENHO/RATE-LIMIT: as batidas são enviadas em LOTE (um POST carrega até
// forwardBatch object_changes), o que derruba o nº de requests de 1-por-batida
// para ~N/forwardBatch — é o que evita o 429 do limitador por IP. O envio honra
// Retry-After e NUNCA aborta o run por rate-limit (ver sendBatch).
//
// É idempotente (o Thera deduplica por device+id+time) e retomável (cursor
// próprio em fullsync.json). No modo histórico total, ao final VERIFICA a
// completude (releitura acima do cursor deve vir vazia) e avança o cursor
// incremental. Registra um record de auditoria.
//
// Só um full-sync roda por vez; enquanto roda, o tick incremental é pulado.
func (c *Collector) FullSync(ctx context.Context, startID int64, fromUnix, toUnix int64, note string) (FullSyncResult, error) {
	return c.Refill(ctx, RefillRequest{StartID: startID, FromUnix: fromUnix, ToUnix: toUnix, Note: note})
}

// Origens de um refill (aparecem no painel, na auditoria e no Thera).
const (
	SourcePanel = "painel"
	SourceTray  = "bandeja"
	SourceThera = "thera"
)

// RefillRequest descreve um refill (full-sync) on-demand.
//   - StartID: 0 = desde o início do que o aparelho guarda; -1 = retomar o
//     refill interrompido; >0 = a partir deste id.
//   - FromUnix/ToUnix (epoch s, 0 = sem limite): só envia batidas do período.
//   - Source: quem pediu (painel, bandeja, thera).
//   - CommandID: comando remoto do Thera a confirmar (ack). Vazio = local.
type RefillRequest struct {
	StartID   int64
	FromUnix  int64
	ToUnix    int64
	Note      string
	Source    string
	CommandID string
}

// ErrRefillRunning é devolvido quando já há um refill em andamento.
var ErrRefillRunning = errors.New("um refill já está em andamento")

// Refill executa o full-sync descrito em req (ver FullSync para a mecânica).
func (c *Collector) Refill(ctx context.Context, req RefillRequest) (FullSyncResult, error) {
	if !c.fullSyncRunning.CompareAndSwap(false, true) {
		return FullSyncResult{}, ErrRefillRunning
	}
	defer c.fullSyncRunning.Store(false)
	return c.runRefill(ctx, req)
}

// runRefill executa o refill; o chamador já reservou fullSyncRunning.
func (c *Collector) runRefill(ctx context.Context, req RefillRequest) (FullSyncResult, error) {
	startID, fromUnix, toUnix, note := req.StartID, req.FromUnix, req.ToUnix, req.Note
	if startID < 0 {
		// Retomar: só existe se houver progresso salvo; e retoma com o período
		// original (senão um refill "de hoje" retomado viraria o histórico todo).
		if !c.ResumableRefill() {
			return FullSyncResult{}, ErrNothingToResume
		}
		if fromUnix == 0 && toUnix == 0 {
			fromUnix, toUnix = c.loadRefillPeriod()
		}
	}
	if note == "" && req.Source != "" {
		note = "disparado pelo " + req.Source
		if req.Source == SourceThera {
			note = "disparado pelo Thera"
		} else if req.Source == SourceTray {
			note = "disparado pela bandeja"
		}
	}

	periodActive := fromUnix > 0 || toUnix > 0

	began := time.Now()
	shownStart := startID
	if startID < 0 {
		shownStart = c.fullCursor.Get()
	}
	if c.st != nil {
		c.st.FullSyncStart(state.FullSyncInfo{Source: req.Source, StartID: shownStart, FromUnix: fromUnix, ToUnix: toUnix})
	}
	c.ackCommand(ctx, req.CommandID, thera.AckRunning, nil, "")
	if periodActive {
		c.log.Infof("full-sync iniciado (startID=%d, período %d..%d) %s", startID, fromUnix, toUnix, note)
	} else {
		c.log.Infof("full-sync iniciado (startID=%d) %s", startID, note)
	}

	rec := audit.Record{Mode: audit.ModeFull, Note: note, FromUnix: fromUnix, ToUnix: toUnix}

	finish := func(r audit.Record, err error) (audit.Record, error) {
		r.DurationMs = time.Since(began).Milliseconds()
		if err != nil && !errors.Is(err, context.Canceled) {
			r.Err = err.Error()
		}
		if errors.Is(err, context.Canceled) {
			r.Mode = audit.ModeFullPartial
			r.Err = "interrompido (retomável)"
		}
		r.Time = time.Now().UTC()
		c.writeAudit(r)
		if c.st != nil {
			c.st.Forwarded(r.Forwarded)
			summary := fmt.Sprintf("ids %d→%d · %d enviadas · %d sem time · verif=%v",
				r.FromID, r.ToID, r.Forwarded, r.SkippedNoTime, r.Verified)
			if r.Err != "" {
				summary = "ERRO: " + r.Err + " · " + summary
			}
			c.st.FullSyncDone(summary, r)
		}
		// O ack final usa um contexto próprio: mesmo num shutdown (ctx cancelado)
		// o Thera fica sabendo que o refill parou e pode ser retomado.
		ackCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		status := thera.AckDone
		if r.Err != "" {
			status = thera.AckFailed
		}
		c.ackCommand(ackCtx, req.CommandID, status, &thera.CommandResult{
			FromID: r.FromID, ToID: r.ToID, Read: r.Read, Forwarded: r.Forwarded,
			SkippedNoTime: r.SkippedNoTime, SkippedBadID: r.SkippedBadID, NoRegistration: r.NoRegistration,
			RotatedGap: r.RotatedGap, DeviceMinID: r.DeviceMinID, Verified: r.Verified, DurationMs: r.DurationMs,
		}, r.Err)
		cancel()
		c.log.Infof("full-sync concluído: ids %d→%d, %d enviadas, %d sem time, %d id inválido, verif=%v, dur=%dms",
			r.FromID, r.ToID, r.Forwarded, r.SkippedNoTime, r.SkippedBadID, r.Verified, r.DurationMs)
		return r, err
	}

	if err := c.device.EnsureSession(ctx); err != nil {
		c.deviceFail(err)
		return finish(rec, err)
	}
	c.deviceOK()

	// Cursor retomável próprio: startID>=0 reinicia a partir dali; <0 retoma.
	var cursor int64
	if startID >= 0 {
		cursor = startID
		_ = c.fullCursor.Set(cursor)
		c.saveRefillPeriod(fromUnix, toUnix)
	} else {
		cursor = c.fullCursor.Get()
	}
	rec.FromID = cursor
	startCursor := cursor

	// Menor id ainda no aparelho (detecta rotação do buffer).
	if min, err := c.device.MinAccessLogID(ctx); err == nil {
		rec.DeviceMinID = min
		if startCursor > 0 && min > startCursor+1 {
			rec.RotatedGap = min - startCursor - 1
			c.log.Errorf("full-sync: possível rotação do buffer — ids (%d..%d) não estão mais no aparelho (lacuna de até %d)",
				startCursor+1, min-1, rec.RotatedGap)
		}
	}

	// --- Batelamento: acumula batidas válidas e envia em lotes grandes. ---
	batchMax := c.forwardBatch
	if batchMax < 1 {
		batchMax = 1
	}
	type pend struct {
		id int64
		ch thera.ObjectChange
	}
	buf := make([]pend, 0, batchMax)
	envDeviceId := c.cfg.DeviceIdInt()
	outOfPeriod := 0

	// readPos é a posição de LEITURA (paginação keyset do aparelho): avança a cada
	// batida LIDA. É distinto de `cursor`, a posição de ENVIO confirmado, que só
	// avança quando o lote é aceito (flush) — assim o batelamento não relê a mesma
	// página e, se interromper, retoma do último lote realmente enviado.
	readPos := cursor

	// flush envia o lote acumulado num POST só e SÓ ENTÃO avança o cursor para o
	// último id do lote (não-perda: cursor nunca passa de uma batida não enviada).
	flush := func() error {
		if len(buf) == 0 {
			return nil
		}
		changes := make([]thera.ObjectChange, len(buf))
		for i, p := range buf {
			changes[i] = p.ch
		}
		if err := c.sendBatch(ctx, changes, envDeviceId); err != nil {
			c.theraFail(err)
			return err
		}
		c.theraOK()
		last := buf[len(buf)-1].id
		cursor = last
		_ = c.fullCursor.Set(last)
		rec.ToID = last
		rec.Forwarded += len(buf)
		if c.st != nil {
			c.st.FullSyncProgress(cursor, int64(rec.Forwarded), int64(rec.Read))
		}
		buf = buf[:0]
		return nil
	}

	// skipAdvance pula uma batida que NÃO será enviada (sem time, ou fora do
	// período), avançando o cursor com segurança: descarrega o lote pendente
	// ANTES (senão o cursor passaria por cima de batidas ainda não enviadas).
	skipAdvance := func(id int64) error {
		if err := flush(); err != nil {
			return err
		}
		cursor = id
		_ = c.fullCursor.Set(id)
		rec.ToID = id
		return nil
	}

	for {
		select {
		case <-ctx.Done():
			return finish(rec, ctx.Err())
		default:
		}

		page, err := c.device.LoadAccessLogsPage(ctx, readPos, c.batchSize)
		if err != nil {
			c.deviceFail(err)
			return finish(rec, err)
		}
		c.deviceOK()
		if len(page) == 0 {
			break // drenou tudo
		}
		sortLogsByID(page)

		// Traduz user_id -> matrícula em lote (uma consulta cobre a página).
		pageUserIds := make([]string, 0, len(page))
		for _, l := range page {
			pageUserIds = append(pageUserIds, string(l.UserId))
		}
		c.device.ResolveRegistrations(ctx, pageUserIds)

		progressed := false
		for _, l := range page {
			select {
			case <-ctx.Done():
				return finish(rec, ctx.Err())
			default:
			}

			rec.Read++
			id, iderr := l.IdNum()
			if iderr != nil {
				rec.SkippedBadID++
				c.log.Errorf("full-sync: id inválido ignorado: id=%q user=%q", l.Id, l.UserId)
				continue
			}
			readPos = id // avança a leitura (paginação) mesmo quando a batida é pulada

			// TRAVA ANTI-CORRUPÇÃO (igual ao incremental): sem time, nunca envia.
			n, ok := validUnixTime(string(l.Time))
			if !ok {
				if err := skipAdvance(id); err != nil {
					return finish(rec, err)
				}
				rec.SkippedNoTime++
				c.log.Errorf("full-sync: batida SEM time válido RECUSADA: id=%s user=%s time=%q", l.Id, l.UserId, l.Time)
				progressed = true
				continue
			}

			// Filtro de período: fora do intervalo, pula (mas avança o cursor).
			if periodActive && !inPeriod(n, fromUnix, toUnix) {
				if err := skipAdvance(id); err != nil {
					return finish(rec, err)
				}
				outOfPeriod++
				progressed = true
				continue
			}

			// Dentro do período: acumula no lote (mesmo aparelho → mesmo envelope).
			envDeviceId = c.envDeviceIdFor(l)
			ch := c.buildChange(l)
			if ch.Values.Registration == "" {
				rec.NoRegistration++
			}
			buf = append(buf, pend{id: id, ch: ch})
			progressed = true
			if len(buf) >= batchMax {
				if err := flush(); err != nil {
					return finish(rec, err)
				}
			}
		}
		// Progresso por PÁGINA LIDA (não só por lote enviado): num refill por data,
		// as páginas fora do período não enviam nada e a barra ficava parada.
		if c.st != nil {
			c.st.FullSyncProgress(readPos, int64(rec.Forwarded), int64(rec.Read))
		}
		// Guard anti-loop-infinito: página inteira só de ids inválidos.
		if !progressed {
			c.log.Errorf("full-sync: página sem progresso (todos ids inválidos) — encerrando para não repetir")
			break
		}
	}

	// Descarrega o que sobrou no lote.
	if err := flush(); err != nil {
		return finish(rec, err)
	}

	// Modo histórico total: verifica completude e avança o cursor incremental.
	// No modo período NÃO fazemos isso (o intervalo é um recorte do passado).
	if !periodActive {
		if verifyPage, verr := c.device.LoadAccessLogsPage(ctx, cursor, 1); verr == nil {
			rec.Verified = len(verifyPage) == 0
		}
		if cursor > c.cursor.Get() {
			if err := c.cursor.Set(cursor); err == nil && c.st != nil {
				c.st.SetCursor(cursor)
			}
		}
	}
	// Full-sync concluído: zera o cursor retomável (e o período salvo).
	_ = c.fullCursor.Clear()
	_ = os.Remove(filepath.Join(c.dir, fullSyncPeriodFile))

	if periodActive && outOfPeriod > 0 {
		c.log.Infof("full-sync período: %d batida(s) fora do intervalo ignoradas", outOfPeriod)
	}
	if rec.NoRegistration > 0 {
		c.log.Errorf("refill: %d batida(s) enviadas SEM matrícula no aparelho — o Thera só as aceita se houver mapa user_id→colaborador; confira o cadastro no Control iD", rec.NoRegistration)
	}

	return finish(rec, nil)
}

// FullSyncRunning diz se um full-sync está em andamento.
func (c *Collector) FullSyncRunning() bool { return c.fullSyncRunning.Load() }

// ErrNothingToResume: pediram "retomar" sem refill interrompido salvo.
var ErrNothingToResume = errors.New("não há refill interrompido para retomar")

// ResumableRefill diz se há um refill interrompido com progresso salvo.
func (c *Collector) ResumableRefill() bool {
	_, err := os.Stat(filepath.Join(c.dir, fullSyncCursorFile))
	return err == nil
}

type refillPeriod struct {
	FromUnix int64 `json:"fromUnix"`
	ToUnix   int64 `json:"toUnix"`
}

func (c *Collector) saveRefillPeriod(fromUnix, toUnix int64) {
	data, _ := json.Marshal(refillPeriod{FromUnix: fromUnix, ToUnix: toUnix})
	_ = os.WriteFile(filepath.Join(c.dir, fullSyncPeriodFile), data, 0o644)
}

func (c *Collector) loadRefillPeriod() (int64, int64) {
	data, err := os.ReadFile(filepath.Join(c.dir, fullSyncPeriodFile))
	if err != nil {
		return 0, 0
	}
	var p refillPeriod
	if json.Unmarshal(data, &p) != nil {
		return 0, 0
	}
	return p.FromUnix, p.ToUnix
}

// StartRefill dispara um refill em segundo plano, preso ao ciclo de vida do
// coletor (o desligamento espera ele encerrar e confirmar). Devolve na hora o
// erro de "já está rodando" ou "nada para retomar".
func (c *Collector) StartRefill(req RefillRequest) error {
	if req.StartID < 0 && !c.ResumableRefill() {
		return ErrNothingToResume
	}
	c.ctxMu.Lock()
	ctx := c.runCtx
	c.ctxMu.Unlock()
	if ctx == nil || ctx.Err() != nil {
		// Coletor ainda não começou ou já está saindo (recarga da config): o
		// pedido iria para um coletor morto e viraria "interrompido" à toa.
		return ErrCollectorRestarting
	}
	return c.goRefill(ctx, req)
}

// ErrCollectorRestarting: o coletor está reiniciando (recarga) — tente de novo.
var ErrCollectorRestarting = errors.New("o coletor está reiniciando; tente de novo em alguns segundos")

// goRefill RESERVA o refill na hora (sem corrida entre dois pedidos) e o roda em
// segundo plano, rastreado para o desligamento esperar e confirmar ao Thera.
func (c *Collector) goRefill(ctx context.Context, req RefillRequest) error {
	if !c.fullSyncRunning.CompareAndSwap(false, true) {
		return ErrRefillRunning
	}
	// Timeout generoso: um histórico grande pode levar dezenas de minutos.
	rctx, cancel := context.WithTimeout(ctx, 90*time.Minute)
	c.ctxMu.Lock()
	c.refillCancel = cancel
	c.ctxMu.Unlock()
	c.refills.Add(1)
	go func() {
		defer c.refills.Done()
		defer cancel()
		defer c.fullSyncRunning.Store(false)
		if _, err := c.runRefill(rctx, req); err != nil && !errors.Is(err, context.Canceled) {
			c.log.Errorf("refill (%s) terminou com erro: %v", orDefault(req.Source, "painel"), err)
		}
	}()
	return nil
}

// CancelRefill interrompe o refill em andamento (fica retomável e o Thera é
// avisado). Sem refill rodando, não faz nada.
func (c *Collector) CancelRefill() bool {
	if !c.fullSyncRunning.Load() {
		return false
	}
	c.ctxMu.Lock()
	cancel := c.refillCancel
	c.ctxMu.Unlock()
	if cancel == nil {
		return false
	}
	c.log.Infof("refill interrompido a pedido (fica retomável)")
	cancel()
	return true
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// waitRefills espera os refills em andamento encerrarem (até timeout).
func (c *Collector) waitRefills(timeout time.Duration) {
	done := make(chan struct{})
	go func() { c.refills.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(timeout):
		c.log.Errorf("refill não encerrou em %s no desligamento; o progresso fica salvo para retomar", timeout)
	}
}

// Run roda o loop principal até o contexto ser cancelado (shutdown limpo do
// serviço). Entre ticks, dorme pollSeconds respeitando ctx.Done(). Se um
// UpdateHook estiver configurado, checa novas versões nos intervalos
// definidos, sempre num ponto seguro do ciclo (fora de um forward em voo).
func (c *Collector) Run(ctx context.Context) {
	c.ctxMu.Lock()
	c.runCtx = ctx
	c.ctxMu.Unlock()
	// Ao sair (desligamento, recarga, update), espera o refill em andamento
	// encerrar e confirmar ao Thera — o ctx dele deriva deste e já foi cancelado.
	defer c.waitRefills(refillShutdownWait)
	c.log.Infof("Coletor PontoPar iniciado · aparelho=%s · thera=%s · poll=%ds · lote=%d",
		c.cfg.DeviceBase(), c.cfg.TheraBase, c.cfg.Poll(), c.batchSize)

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
			if c.st != nil {
				c.st.RecordError(err.Error())
			}
			// Força relogin no próximo tick (igual session=null do Node).
			c.device.ClearSession()
		}

		// Ponto seguro: sem batida em voo. Consulta comandos on-demand (ex.:
		// full-sync por período disparado pela web) e dispara se houver.
		c.maybeRunQueuedFullSync(ctx)

		// Ponto seguro: sem batida em voo. Checa update se estiver na hora.
		// Nunca durante um refill: o restart do update mataria o refill no meio.
		if c.updater != nil && c.updateEvery > 0 && !nextUpdateCheck.IsZero() && time.Now().After(nextUpdateCheck) && !c.fullSyncRunning.Load() {
			nextUpdateCheck = time.Now().Add(c.updateEvery)
			if c.checkUpdate(ctx) {
				// Update aplicado: o loop encerra e o onUpdated decide o restart.
				return
			}
		}

		select {
		case <-ctx.Done():
			c.log.Infof("coletor finalizando (contexto cancelado)")
			if c.st != nil {
				c.st.Flush()
			}
			return
		case <-time.After(poll):
		case <-c.wake:
			c.log.Infof("coleta imediata solicitada")
		}
	}
}

// CollectNow acorda o loop para coletar já, sem esperar o pollSeconds. Não
// bloqueia; pedidos repetidos enquanto um está pendente são agregados.
func (c *Collector) CollectNow() {
	if c.st != nil {
		c.st.CollectNowRequested()
	}
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

// ackCommand confirma um comando remoto ao Thera (no-op sem commandID). O ack
// FINAL (done/failed) é retentado com backoff: perder ele fazia o Thera achar
// que o coletor reiniciou no meio, mesmo com o refill concluído.
func (c *Collector) ackCommand(ctx context.Context, commandID, status string, result *thera.CommandResult, errMsg string) {
	if commandID == "" {
		return
	}
	attempts := 1
	if status != thera.AckRunning {
		attempts = 5
	}
	wait := time.Second
	var err error
	for i := 0; i < attempts; i++ {
		if err = c.thera.AckCommand(ctx, commandID, status, result, errMsg); err == nil {
			return
		}
		if i == attempts-1 {
			break
		}
		select {
		case <-ctx.Done():
			i = attempts
		case <-time.After(wait):
			wait *= 2
		}
	}
	c.log.Errorf("não consegui confirmar o comando %s ao Thera (%s): %v", commandID, status, err)
}

// maybeRunQueuedFullSync consulta o Thera por comandos on-demand (full-sync
// disparado pela web) e, se houver um pendente, dispara o full-sync em segundo
// plano (o tick pula a coleta incremental enquanto o full-sync roda). É
// best-effort e barata: gated por commandEvery, nunca derruba o ciclo por falha
// de rede/endpoint ausente (servidor antigo sem /commands), e não dispara se já
// houver um full-sync em andamento.
func (c *Collector) maybeRunQueuedFullSync(ctx context.Context) {
	if c.commandEvery <= 0 {
		return
	}
	if !c.lastCommandCheck.IsZero() && time.Since(c.lastCommandCheck) < c.commandEvery {
		return
	}
	c.lastCommandCheck = time.Now()
	if c.fullSyncRunning.Load() {
		return // já rodando: nem consulta (o comando read-once fica pro próximo)
	}
	cmds, err := c.thera.GetCommands(ctx)
	if err != nil {
		// Sem barulho: endpoint pode não existir no servidor ainda, ou rede caiu.
		// A coleta normal não depende disto.
		return
	}
	fs := cmds.PendingFullSync
	if fs == nil {
		return
	}
	note := "disparado pelo Thera"
	if fs.FromUnix > 0 || fs.ToUnix > 0 {
		note = "disparado pelo Thera (período)"
	}
	c.log.Infof("comando do Thera: refill (startId=%d, período %d..%d, comando=%s) — iniciando", fs.StartID, fs.FromUnix, fs.ToUnix, fs.CommandID)
	req := RefillRequest{StartID: fs.StartID, FromUnix: fs.FromUnix, ToUnix: fs.ToUnix, Note: note, Source: SourceThera, CommandID: fs.CommandID}
	if fs.StartID < 0 && !c.ResumableRefill() {
		// Pediram retomar mas não há o que retomar aqui: faz o refill do início
		// (com o período pedido), que é o que o RH quer de fato.
		req.StartID = 0
	}
	if err := c.goRefill(ctx, req); err != nil {
		c.log.Errorf("refill pedido pelo Thera não iniciou: %v", err)
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
