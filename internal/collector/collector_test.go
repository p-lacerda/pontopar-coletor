package collector

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/p-lacerda/pontopar-coletor/internal/audit"
	"github.com/p-lacerda/pontopar-coletor/internal/config"
	"github.com/p-lacerda/pontopar-coletor/internal/idface"
	"github.com/p-lacerda/pontopar-coletor/internal/state"
)

type nopLogger struct{ t *testing.T }

func (nopLogger) Infof(string, ...any)  {}
func (nopLogger) Errorf(string, ...any) {}

type fakeLog struct {
	ID   int64
	Time string
	User string
}

// fakeDevice simula o iDFace: login/sessão, min id, paginação keyset por id>X
// (event=7) e o de-para users.
func fakeDevice(t *testing.T, logs []fakeLog, regByUser map[string]string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/login.fcgi"):
			_, _ = w.Write([]byte(`{"session":"sess"}`))
		case strings.HasPrefix(r.URL.Path, "/session_is_valid.fcgi"):
			_, _ = w.Write([]byte(`{"session_is_valid":true}`))
		case strings.HasPrefix(r.URL.Path, "/load_objects.fcgi"):
			body, _ := io.ReadAll(r.Body)
			var req struct {
				Object string           `json:"object"`
				Fields []string         `json:"fields"`
				Where  []map[string]any `json:"where"`
				Limit  int              `json:"limit"`
			}
			_ = json.Unmarshal(body, &req)
			if req.Object == "users" {
				// Responde o de-para dos ids pedidos (where encadeado).
				var users []map[string]any
				for _, cond := range req.Where {
					if v, ok := cond["value"]; ok {
						id := strconv.FormatInt(int64(v.(float64)), 10)
						if reg, ok := regByUser[id]; ok {
							users = append(users, map[string]any{"id": id, "registration": reg})
						}
					}
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"users": users})
				return
			}
			// access_logs: sem where => consulta de min id; com where => keyset.
			sorted := append([]fakeLog(nil), logs...)
			sort.Slice(sorted, func(i, j int) bool { return sorted[i].ID < sorted[j].ID })
			if len(req.Where) == 0 {
				if len(sorted) == 0 {
					_, _ = w.Write([]byte(`{"access_logs":[]}`))
					return
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"access_logs": []map[string]any{{"id": sorted[0].ID}}})
				return
			}
			var after int64
			if v, ok := req.Where[0]["value"].(float64); ok {
				after = int64(v)
			}
			var out []map[string]any
			for _, l := range sorted {
				if l.ID <= after {
					continue
				}
				out = append(out, map[string]any{"id": l.ID, "time": l.Time, "event": "7", "device_id": "4409419584542362", "user_id": l.User})
				if req.Limit > 0 && len(out) >= req.Limit {
					break
				}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"access_logs": out})
		default:
			w.WriteHeader(404)
		}
	}))
}

// fakeThera captura todos os /dao recebidos.
type fakeThera struct {
	srv      *httptest.Server
	mu       sync.Mutex
	dao      []map[string]string // id -> time, por envio (lista de values)
	calls    int                 // nº de REQUISIÇÕES /dao (p/ provar o batelamento)
	pending  string              // JSON do pendingFullSync servido em /commands (read-once)
	cmdReads int                 // nº de leituras de /commands
	acks     []map[string]any    // corpos recebidos em /commands/ack
	daoDelay time.Duration       // atraso artificial no /dao (simula Thera lento)
}

func (ft *fakeThera) ackList() []map[string]any {
	ft.mu.Lock()
	defer ft.mu.Unlock()
	return append([]map[string]any(nil), ft.acks...)
}

func newFakeThera() *fakeThera {
	ft := &fakeThera{}
	ft.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/dao") {
			ft.mu.Lock()
			delay := ft.daoDelay
			ft.mu.Unlock()
			if delay > 0 {
				select {
				case <-time.After(delay):
				case <-r.Context().Done():
					return
				}
			}
			body, _ := io.ReadAll(r.Body)
			var p struct {
				ObjectChanges []struct {
					Values map[string]string `json:"values"`
				} `json:"object_changes"`
			}
			_ = json.Unmarshal(body, &p)
			ft.mu.Lock()
			ft.calls++
			for _, oc := range p.ObjectChanges {
				ft.dao = append(ft.dao, oc.Values)
			}
			ft.mu.Unlock()
			_, _ = w.Write([]byte(`{"ok":true}`))
			return
		}
		if strings.HasSuffix(r.URL.Path, "/commands/ack") {
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			ft.mu.Lock()
			ft.acks = append(ft.acks, body)
			ft.mu.Unlock()
			_, _ = w.Write([]byte(`{"ok":true}`))
			return
		}
		if strings.HasSuffix(r.URL.Path, "/commands") {
			ft.mu.Lock()
			ft.cmdReads++
			p := ft.pending
			ft.pending = "" // read-once
			ft.mu.Unlock()
			if p == "" {
				_, _ = w.Write([]byte(`{"pendingFullSync":null}`))
			} else {
				_, _ = w.Write([]byte(`{"pendingFullSync":` + p + `}`))
			}
			return
		}
		w.WriteHeader(200)
	}))
	return ft
}

func (ft *fakeThera) setPending(json string) {
	ft.mu.Lock()
	ft.pending = json
	ft.mu.Unlock()
}

func (ft *fakeThera) sent() []map[string]string {
	ft.mu.Lock()
	defer ft.mu.Unlock()
	return append([]map[string]string(nil), ft.dao...)
}

func (ft *fakeThera) reqCount() int {
	ft.mu.Lock()
	defer ft.mu.Unlock()
	return ft.calls
}

func newTestCollector(t *testing.T, deviceURL, theraURL string) (*Collector, *state.State, *audit.Log) {
	dir := t.TempDir()
	cfg := &config.Config{
		DeviceIp: "x", DevicePort: 90, Login: "admin", Password: "admin",
		DeviceId: 0, DeviceSecret: "s", TheraBase: theraURL, BatchSize: 2,
	}
	cfg.SetDeviceId(4409419584542362)
	st := state.New(dir, "test")
	au := audit.New(dir)
	c := New(cfg, dir, nopLogger{t: t}, st, au)
	// Reaponta o cliente do device para o servidor fake (o cliente do Thera já
	// foi montado a partir de cfg.TheraBase = theraURL em New()).
	c.device = idface.New(deviceURL, "admin", "admin", &http.Client{Timeout: 5 * time.Second}, nopLogger{t: t})
	return c, st, au
}

func TestFullSyncCarriesRealTimeAndRefusesMissingTime(t *testing.T) {
	logs := []fakeLog{
		{ID: 10, Time: "1700000010", User: "100"},
		{ID: 11, Time: "1700000011", User: "100"},
		{ID: 12, Time: "", User: "100"}, // SEM time -> deve ser RECUSADA
		{ID: 13, Time: "1700000013", User: "100"},
	}
	dev := fakeDevice(t, logs, map[string]string{"100": "0001"})
	defer dev.Close()
	ft := newFakeThera()
	defer ft.srv.Close()

	c, st, au := newTestCollector(t, dev.URL, ft.srv.URL)

	rec, err := c.FullSync(context.Background(), 0, 0, 0, "teste")
	if err != nil {
		t.Fatalf("FullSync erro: %v", err)
	}
	if rec.Forwarded != 3 {
		t.Errorf("forwarded: quero 3, veio %d", rec.Forwarded)
	}
	if rec.SkippedNoTime != 1 {
		t.Errorf("skippedNoTime: quero 1 (id 12), veio %d", rec.SkippedNoTime)
	}
	if rec.ToID != 13 {
		t.Errorf("toId: quero 13, veio %d", rec.ToID)
	}
	if !rec.Verified {
		t.Errorf("full-sync deveria terminar verificado (releitura vazia)")
	}

	sent := ft.sent()
	if len(sent) != 3 {
		t.Fatalf("dao recebidos: quero 3, veio %d", len(sent))
	}
	// Cada batida enviada carrega o time REAL do evento (nunca colapsado/vazio).
	byID := map[string]string{}
	for _, v := range sent {
		if v["time"] == "" {
			t.Fatalf("batida enviada SEM time: %+v (colapsaria o horário no backend)", v)
		}
		if v["registration"] != "0001" {
			t.Errorf("matrícula errada: %+v", v)
		}
		byID[v["id"]] = v["time"]
	}
	if byID["10"] != "1700000010" || byID["11"] != "1700000011" || byID["13"] != "1700000013" {
		t.Errorf("times não batem com o device: %+v", byID)
	}
	if _, ok := byID["12"]; ok {
		t.Errorf("id 12 (sem time) jamais deveria ter sido enviado")
	}

	// Cursor incremental avançou para 13 (não reprocessa tudo depois).
	if st.Get().Cursor != 13 {
		t.Errorf("cursor incremental: quero 13, veio %d", st.Get().Cursor)
	}
	// Auditoria registrou o run completo.
	recs, _ := au.Recent(10)
	if len(recs) == 0 || recs[len(recs)-1].Mode != audit.ModeFull {
		t.Errorf("auditoria do full-sync ausente: %+v", recs)
	}
}

func TestFullSyncResumeAndIdempotent(t *testing.T) {
	logs := []fakeLog{
		{ID: 1, Time: "1700000001", User: "100"},
		{ID: 2, Time: "1700000002", User: "100"},
	}
	dev := fakeDevice(t, logs, map[string]string{"100": "7"})
	defer dev.Close()
	ft := newFakeThera()
	defer ft.srv.Close()
	c, _, _ := newTestCollector(t, dev.URL, ft.srv.URL)

	if _, err := c.FullSync(context.Background(), 0, 0, 0, ""); err != nil {
		t.Fatal(err)
	}
	// Re-rodar reenvia tudo (o backend é quem deduplica — o coletor sempre carrega o time real).
	if _, err := c.FullSync(context.Background(), 0, 0, 0, ""); err != nil {
		t.Fatal(err)
	}
	if n := len(ft.sent()); n != 4 {
		t.Fatalf("dois full-syncs de 2 batidas deveriam enviar 4 (dedupe é do backend), veio %d", n)
	}
}

// TestFullSyncBatchesAndFiltersPeriod prova as duas garantias novas: (1) envio em
// LOTE — várias batidas numa única requisição /dao (poucas requests → sem 429); e
// (2) recorte por PERÍODO — só as batidas dentro do intervalo são enviadas.
func TestFullSyncBatchesAndFiltersPeriod(t *testing.T) {
	logs := []fakeLog{
		{ID: 1, Time: "1700000001", User: "100"}, // antes do período
		{ID: 2, Time: "1700000100", User: "100"}, // antes do período
		{ID: 3, Time: "1700000200", User: "100"}, // DENTRO
		{ID: 4, Time: "1700000300", User: "100"}, // DENTRO
		{ID: 5, Time: "1700000400", User: "100"}, // depois do período
	}
	dev := fakeDevice(t, logs, map[string]string{"100": "0001"})
	defer dev.Close()
	ft := newFakeThera()
	defer ft.srv.Close()
	c, st, _ := newTestCollector(t, dev.URL, ft.srv.URL)

	// Período [1700000150, 1700000350] → só ids 3 e 4.
	rec, err := c.FullSync(context.Background(), 0, 1700000150, 1700000350, "período")
	if err != nil {
		t.Fatalf("FullSync erro: %v", err)
	}
	if rec.Forwarded != 2 {
		t.Errorf("forwarded: quero 2 (ids 3,4), veio %d", rec.Forwarded)
	}

	sent := ft.sent()
	if len(sent) != 2 {
		t.Fatalf("enviados: quero 2, veio %d (%+v)", len(sent), sent)
	}
	got := map[string]string{}
	for _, v := range sent {
		got[v["id"]] = v["time"]
	}
	if got["3"] != "1700000200" || got["4"] != "1700000300" {
		t.Errorf("deveria ter enviado só 3 e 4 com time real, veio %+v", got)
	}

	// Batelamento: as 2 batidas foram num ÚNICO POST /dao (não 2 requests).
	if n := ft.reqCount(); n != 1 {
		t.Errorf("batelamento: quero 1 requisição /dao carregando as 2 batidas, veio %d", n)
	}

	// Modo período NÃO mexe no cursor incremental (é um recorte do passado).
	if st.Get().Cursor != 0 {
		t.Errorf("cursor incremental não deveria avançar no modo período, veio %d", st.Get().Cursor)
	}
}

// TestQueuedFullSyncFromWebRunsWithPeriod prova o disparo PELA WEB: o coletor lê
// o comando pendente em /commands e roda o full-sync com o período pedido.
func TestQueuedFullSyncFromWebRunsWithPeriod(t *testing.T) {
	logs := []fakeLog{
		{ID: 1, Time: "1700000001", User: "100"}, // fora (antes)
		{ID: 2, Time: "1700000200", User: "100"}, // DENTRO
		{ID: 3, Time: "1700000300", User: "100"}, // DENTRO
		{ID: 4, Time: "1700000400", User: "100"}, // fora (depois)
	}
	dev := fakeDevice(t, logs, map[string]string{"100": "0001"})
	defer dev.Close()
	ft := newFakeThera()
	defer ft.srv.Close()
	c, _, _ := newTestCollector(t, dev.URL, ft.srv.URL)

	// Comando enfileirado pela web: só o intervalo [150,350] → ids 2 e 3.
	ft.setPending(`{"startId":0,"fromUnix":1700000150,"toUnix":1700000350}`)
	c.maybeRunQueuedFullSync(context.Background())

	// O full-sync roda em goroutine; espera concluir.
	deadline := time.Now().Add(3 * time.Second)
	for c.FullSyncRunning() || len(ft.sent()) == 0 {
		if time.Now().After(deadline) {
			t.Fatalf("full-sync disparado pela web não concluiu a tempo (enviados=%d)", len(ft.sent()))
		}
		time.Sleep(15 * time.Millisecond)
	}
	sent := ft.sent()
	got := map[string]bool{}
	for _, v := range sent {
		got[v["id"]] = true
	}
	if !got["2"] || !got["3"] || got["1"] || got["4"] || len(sent) != 2 {
		t.Fatalf("esperava enviar só ids 2 e 3 (período), veio %+v", sent)
	}
	if ft.cmdReads == 0 {
		t.Errorf("o coletor deveria ter lido /commands")
	}
	// Segunda checada: comando já foi consumido (read-once) → não dispara de novo.
	c.lastCommandCheck = time.Time{} // libera o gate
	c.maybeRunQueuedFullSync(context.Background())
	time.Sleep(50 * time.Millisecond)
	if len(ft.sent()) != 2 {
		t.Errorf("comando read-once não deveria re-disparar; enviados=%d", len(ft.sent()))
	}
}

func TestIncrementalTickRefusesMissingTime(t *testing.T) {
	logs := []fakeLog{
		{ID: 5, Time: "", User: "100"},           // sem time -> recusada, cursor avança
		{ID: 6, Time: "1700000006", User: "100"}, // enviada
	}
	dev := fakeDevice(t, logs, map[string]string{"100": "0001"})
	defer dev.Close()
	ft := newFakeThera()
	defer ft.srv.Close()
	c, st, _ := newTestCollector(t, dev.URL, ft.srv.URL)
	// Evita o syncUsers (manifesto) no tick: força lastSync recente.
	c.lastSync = time.Now()
	c.lastClockSync = time.Now()

	if err := c.tick(context.Background()); err != nil {
		t.Fatalf("tick erro: %v", err)
	}
	sent := ft.sent()
	if len(sent) != 1 || sent[0]["id"] != "6" {
		t.Fatalf("só a batida 6 (com time) deveria ser enviada, veio %+v", sent)
	}
	if st.Get().Cursor != 6 {
		t.Errorf("cursor deveria avançar para 6 (pulando a 5 sem time), veio %d", st.Get().Cursor)
	}
	if st.Get().SkippedNoTime != 1 {
		t.Errorf("skippedNoTime no estado: quero 1, veio %d", st.Get().SkippedNoTime)
	}
}

// Um refill pedido pelo Thera com commandId é confirmado: "running" no início e
// "done" com o resultado (lidas/enviadas/sem matrícula) no fim — é o que deixa o
// RH ver no Thera que o refill rodou e o que ele encontrou.
func TestRemoteRefillAcksRunningAndDoneWithResult(t *testing.T) {
	logs := []fakeLog{
		{ID: 1, Time: "1700000100", User: "100"},
		{ID: 2, Time: "1700000200", User: "999"}, // sem matrícula no aparelho
		{ID: 3, Time: "1700000300", User: "100"},
	}
	dev := fakeDevice(t, logs, map[string]string{"100": "0003"})
	defer dev.Close()
	ft := newFakeThera()
	defer ft.srv.Close()
	c, st, _ := newTestCollector(t, dev.URL, ft.srv.URL)

	ft.setPending(`{"startId":0,"fromUnix":0,"toUnix":0,"commandId":"cmd-42"}`)
	c.maybeRunQueuedFullSync(context.Background())

	deadline := time.Now().Add(3 * time.Second)
	for len(ft.ackList()) < 2 {
		if time.Now().After(deadline) {
			t.Fatalf("esperava 2 acks (running, done), veio %+v", ft.ackList())
		}
		time.Sleep(15 * time.Millisecond)
	}
	acks := ft.ackList()
	if acks[0]["commandId"] != "cmd-42" || acks[0]["status"] != "running" {
		t.Errorf("primeiro ack deveria ser running do cmd-42: %+v", acks[0])
	}
	if acks[1]["status"] != "done" {
		t.Fatalf("segundo ack deveria ser done: %+v", acks[1])
	}
	res, _ := acks[1]["result"].(map[string]any)
	if res["read"] != float64(3) || res["forwarded"] != float64(3) || res["noRegistration"] != float64(1) {
		t.Errorf("resultado no ack inesperado: %+v", res)
	}
	snap := st.Get()
	if snap.FullSyncLast == nil || snap.FullSyncLast.NoRegistration != 1 || snap.FullSyncSource != SourceThera {
		t.Errorf("estado do último refill inesperado: %+v (origem %q)", snap.FullSyncLast, snap.FullSyncSource)
	}
}

// Sem commandId (servidor antigo, fila em memória), nada de ack.
func TestRemoteRefillWithoutCommandIDDoesNotAck(t *testing.T) {
	dev := fakeDevice(t, []fakeLog{{ID: 1, Time: "1700000100", User: "100"}}, map[string]string{"100": "1"})
	defer dev.Close()
	ft := newFakeThera()
	defer ft.srv.Close()
	c, _, _ := newTestCollector(t, dev.URL, ft.srv.URL)
	ft.setPending(`{"startId":0,"fromUnix":0,"toUnix":0}`)
	c.maybeRunQueuedFullSync(context.Background())
	deadline := time.Now().Add(3 * time.Second)
	for c.FullSyncRunning() || len(ft.sent()) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("refill não concluiu")
		}
		time.Sleep(15 * time.Millisecond)
	}
	time.Sleep(30 * time.Millisecond)
	if n := len(ft.ackList()); n != 0 {
		t.Fatalf("não deveria confirmar sem commandId; acks=%d", n)
	}
}

// "Coletar agora" acorda o loop sem esperar o pollSeconds, e a batida entra no
// feed do painel com a matrícula e na contagem do dia.
func TestCollectNowWakesLoopAndFeedsRecentPunches(t *testing.T) {
	now := time.Now().Unix()
	logs := []fakeLog{{ID: 7, Time: strconv.FormatInt(now, 10), User: "100"}}
	dev := fakeDevice(t, logs, map[string]string{"100": "0003"})
	defer dev.Close()
	ft := newFakeThera()
	defer ft.srv.Close()
	c, st, _ := newTestCollector(t, dev.URL, ft.srv.URL)
	c.cfg.PollSeconds = 3600 // sem o wake, a 2ª volta só viria em 1h
	c.lastSync = time.Now()
	c.lastClockSync = time.Now()
	c.commandEvery = 0

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { c.Run(ctx); close(done) }()

	waitFor := func(cond func() bool, what string) {
		deadline := time.Now().Add(3 * time.Second)
		for !cond() {
			if time.Now().After(deadline) {
				t.Fatalf("timeout esperando %s", what)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	waitFor(func() bool { return len(ft.sent()) == 1 }, "primeiro tick")
	ticks := st.Get().LastTickAt
	c.CollectNow()
	waitFor(func() bool { return st.Get().LastTickAt.After(ticks) }, "tick acordado por CollectNow")

	snap := st.Get()
	if len(snap.RecentPunches) != 1 || snap.RecentPunches[0].Registration != "0003" || snap.RecentPunches[0].ID != 7 {
		t.Errorf("feed de últimas batidas inesperado: %+v", snap.RecentPunches)
	}
	if snap.ForwardedToday != 1 {
		t.Errorf("batidas hoje: quero 1, veio %d", snap.ForwardedToday)
	}
	cancel()
	<-done
}

// Desligar o serviço no meio de um refill remoto: o coletor espera o refill
// encerrar e CONFIRMA "failed" ao Thera antes de Run voltar — senão o comando
// ficava RUNNING por horas e o botão no Thera travado.
func TestShutdownDuringRemoteRefillAcksFailedBeforeRunReturns(t *testing.T) {
	var logs []fakeLog
	for i := 1; i <= 40; i++ {
		logs = append(logs, fakeLog{ID: int64(i), Time: strconv.Itoa(1700000000 + i), User: "100"})
	}
	dev := fakeDevice(t, logs, map[string]string{"100": "1"})
	defer dev.Close()
	ft := newFakeThera()
	defer ft.srv.Close()
	ft.daoDelay = 300 * time.Millisecond
	c, _, _ := newTestCollector(t, dev.URL, ft.srv.URL)
	c.forwardBatch = 2 // muitos POSTs lentos → dá tempo de desligar no meio
	c.lastSync, c.lastClockSync = time.Now(), time.Now()
	_ = c.cursor.Set(40) // a coleta normal já está em dia: o 1º ciclo não envia nada
	ft.setPending(`{"startId":0,"fromUnix":0,"toUnix":0,"commandId":"cmd-shut"}`)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { c.Run(ctx); close(done) }()
	deadline := time.Now().Add(3 * time.Second)
	for !c.FullSyncRunning() {
		if time.Now().After(deadline) {
			t.Fatal("refill remoto não começou")
		}
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond)
	cancel() // "serviço parando"
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("Run não voltou após o desligamento")
	}
	var last map[string]any
	for _, a := range ft.ackList() {
		if a["commandId"] == "cmd-shut" {
			last = a
		}
	}
	if last == nil || last["status"] != "failed" {
		t.Fatalf("o último ack antes de Run voltar deveria ser failed; acks=%+v", ft.ackList())
	}
	if !c.ResumableRefill() {
		t.Error("o progresso do refill interrompido deveria ficar salvo para retomar")
	}
}

// "Retomar" sem nada salvo é erro (antes virava um refill total desde o id 0).
func TestResumeWithoutProgressIsRejected(t *testing.T) {
	dev := fakeDevice(t, []fakeLog{{ID: 1, Time: "1700000001", User: "100"}}, nil)
	defer dev.Close()
	ft := newFakeThera()
	defer ft.srv.Close()
	c, _, _ := newTestCollector(t, dev.URL, ft.srv.URL)
	if err := c.StartRefill(RefillRequest{StartID: -1}); !errors.Is(err, ErrNothingToResume) {
		t.Fatalf("esperava ErrNothingToResume, veio %v", err)
	}
	if _, err := c.Refill(context.Background(), RefillRequest{StartID: -1}); !errors.Is(err, ErrNothingToResume) {
		t.Fatalf("Refill direto: esperava ErrNothingToResume, veio %v", err)
	}
	if n := len(ft.sent()); n != 0 {
		t.Fatalf("nada deveria ser enviado; enviados=%d", n)
	}
}

// Retomar um refill de PERÍODO continua com o mesmo período (antes virava total).
func TestResumeKeepsOriginalPeriod(t *testing.T) {
	logs := []fakeLog{
		{ID: 1, Time: "1700000100", User: "100"},
		{ID: 2, Time: "1700000200", User: "100"},
		{ID: 3, Time: "1700000300", User: "100"},
		{ID: 4, Time: "1700000400", User: "100"},
	}
	dev := fakeDevice(t, logs, map[string]string{"100": "1"})
	defer dev.Close()
	ft := newFakeThera()
	defer ft.srv.Close()
	c, _, _ := newTestCollector(t, dev.URL, ft.srv.URL)
	// Simula um refill do período [150,350] interrompido logo após o id 1.
	_ = c.fullCursor.Set(1)
	c.saveRefillPeriod(1700000150, 1700000350)
	if _, err := c.Refill(context.Background(), RefillRequest{StartID: -1}); err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, v := range ft.sent() {
		got[v["id"]] = true
	}
	if !got["2"] || !got["3"] || got["4"] || len(ft.sent()) != 2 {
		t.Fatalf("retomada deveria respeitar o período (só 2 e 3), veio %+v", ft.sent())
	}
	if c.ResumableRefill() {
		t.Error("refill concluído não deveria continuar retomável")
	}
}
