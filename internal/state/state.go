// Package state mantém o ESTADO VIVO do coletor (status de conexão, cursor,
// contadores, última sync, progresso do refill) num objeto seguro para uso
// concorrente. O painel e a bandeja leem um Snapshot imutável; o coletor
// atualiza os campos conforme opera.
//
// O estado também é persistido (throttle) em state.json ao lado do executável:
// serve para diagnóstico e para o painel não "zerar" depois de um reinício do
// Windows (batidas de hoje, últimas batidas e o resultado do último refill são
// restaurados no arranque).
package state

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/p-lacerda/pontopar-coletor/internal/audit"
)

// recentMax é quantas batidas recentes o painel mostra no feed.
const recentMax = 25

// Punch é uma batida encaminhada ao Thera, para o feed "últimas batidas".
type Punch struct {
	ID           int64     `json:"id"`
	UserID       string    `json:"userId"`
	Registration string    `json:"registration,omitempty"`
	Time         int64     `json:"time"` // epoch (s) do evento NO APARELHO
	SentAt       time.Time `json:"sentAt"`
}

// Snapshot é uma cópia imutável do estado, segura para serializar/enviar à UI.
type Snapshot struct {
	Version   string    `json:"version"`
	StartedAt time.Time `json:"startedAt"`
	Now       time.Time `json:"now"`

	// Conexão com o aparelho (LAN).
	DeviceOnline  bool      `json:"deviceOnline"`
	DeviceLastOK  time.Time `json:"deviceLastOk"`
	DeviceBase    string    `json:"deviceBase"`
	DeviceLastErr string    `json:"deviceLastError,omitempty"`

	// Conexão com o Thera (nuvem).
	TheraOnline  bool      `json:"theraOnline"`
	TheraLastOK  time.Time `json:"theraLastOk"`
	TheraBase    string    `json:"theraBase"`
	TheraLastErr string    `json:"theraLastError,omitempty"`

	// Progresso da coleta.
	Cursor         int64     `json:"cursor"`         // último id confirmado (incremental)
	MaxDeviceID    int64     `json:"maxDeviceId"`    // maior id já visto no aparelho
	LastTickAt     time.Time `json:"lastTickAt"`     // último ciclo (tentou coletar)
	LastSyncAt     time.Time `json:"lastSyncAt"`     // último ciclo que enviou >=1 batida
	ForwardedTotal int64     `json:"forwardedTotal"` // batidas enviadas (desde o boot)
	SkippedNoTime  int64     `json:"skippedNoTime"`  // recusadas por falta de `time`
	ErrorCount     int64     `json:"errorCount"`
	LastError      string    `json:"lastError,omitempty"`
	LastErrorAt    time.Time `json:"lastErrorAt,omitempty"`

	// Batidas do dia (data LOCAL do evento) recebidas em tempo real, e o feed
	// das últimas batidas encaminhadas.
	TodayDate      string    `json:"todayDate"`
	ForwardedToday int64     `json:"forwardedToday"`
	NoRegistration int64     `json:"noRegistration"` // enviadas sem matrícula (o Thera pode descartar)
	RecentPunches  []Punch   `json:"recentPunches"`
	PollSeconds    int       `json:"pollSeconds"`
	CollectNowAt   time.Time `json:"collectNowAt,omitempty"`

	// Sincronização de usuários/faces (manifesto do Thera).
	LastUserSyncAt  time.Time `json:"lastUserSyncAt,omitempty"`
	UsersObserved   int       `json:"usersObserved"`
	UserSyncError   string    `json:"userSyncError,omitempty"`
	UserSyncErrorAt time.Time `json:"userSyncErrorAt,omitempty"`

	// Refill (full-sync) on-demand.
	FullSyncRunning    bool          `json:"fullSyncRunning"`
	FullSyncStartedAt  time.Time     `json:"fullSyncStartedAt,omitempty"`
	FullSyncSource     string        `json:"fullSyncSource,omitempty"` // painel | bandeja | thera
	FullSyncFromUnix   int64         `json:"fullSyncFromUnix,omitempty"`
	FullSyncToUnix     int64         `json:"fullSyncToUnix,omitempty"`
	FullSyncStartID    int64         `json:"fullSyncStartId"`
	FullSyncTargetID   int64         `json:"fullSyncTargetId"` // estimativa do fim (maior id visto)
	FullSyncCurrentID  int64         `json:"fullSyncCurrentId"`
	FullSyncForwarded  int64         `json:"fullSyncForwarded"`
	FullSyncReadTotal  int64         `json:"fullSyncReadTotal"`
	FullSyncLastResult string        `json:"fullSyncLastResult,omitempty"`
	FullSyncLastAt     time.Time     `json:"fullSyncLastAt,omitempty"`
	FullSyncLast       *audit.Record `json:"fullSyncLast,omitempty"`
	// RefillResumable: há um refill interrompido com progresso salvo (o
	// supervisor preenche na leitura; não é persistido).
	RefillResumable bool `json:"refillResumable"`
	// ReloadPending: config salva, aplicada quando o refill terminar.
	ReloadPending bool `json:"reloadPending"`
}

// State é o estado mutável protegido por mutex.
type State struct {
	mu   sync.Mutex
	snap Snapshot

	path         string
	lastPersist  time.Time
	persistEvery time.Duration
	now          func() time.Time
}

// New cria o estado. dir é onde state.json será gravado. Se já existir um
// state.json (reinício do serviço/Windows), restaura o que faz sentido manter:
// batidas de hoje (se ainda for o mesmo dia), últimas batidas, último refill.
func New(dir, version string) *State {
	s := &State{
		snap:         Snapshot{Version: version, StartedAt: time.Now()},
		path:         filepath.Join(dir, "state.json"),
		persistEvery: 3 * time.Second,
		now:          time.Now,
	}
	s.restore()
	return s
}

func (s *State) restore() {
	data, err := os.ReadFile(s.path)
	if err != nil {
		return
	}
	var old Snapshot
	if json.Unmarshal(data, &old) != nil {
		return
	}
	if old.TodayDate == s.today() {
		s.snap.TodayDate = old.TodayDate
		s.snap.ForwardedToday = old.ForwardedToday
	}
	if len(old.RecentPunches) > recentMax {
		old.RecentPunches = old.RecentPunches[:recentMax]
	}
	s.snap.RecentPunches = old.RecentPunches
	s.snap.LastSyncAt = old.LastSyncAt
	s.snap.MaxDeviceID = old.MaxDeviceID
	s.snap.FullSyncLastResult = old.FullSyncLastResult
	s.snap.FullSyncLastAt = old.FullSyncLastAt
	s.snap.FullSyncLast = old.FullSyncLast
	s.snap.LastUserSyncAt = old.LastUserSyncAt
	s.snap.UsersObserved = old.UsersObserved
}

func (s *State) today() string { return s.now().Format("2006-01-02") }

// Get devolve um Snapshot atual (com Now preenchido). Vira o dia sozinho: se a
// data mudou desde a última batida, "batidas hoje" aparece zerado.
func (s *State) Get() Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	snap := s.snap
	snap.Now = s.now()
	if snap.TodayDate != s.today() {
		snap.TodayDate = s.today()
		snap.ForwardedToday = 0
	}
	snap.RecentPunches = append([]Punch(nil), s.snap.RecentPunches...)
	return snap
}

// mutate aplica fn sob o lock e persiste (throttle).
func (s *State) mutate(fn func(*Snapshot)) {
	s.mu.Lock()
	fn(&s.snap)
	snap := s.snap
	should := time.Since(s.lastPersist) >= s.persistEvery
	if should {
		s.lastPersist = time.Now()
	}
	s.mu.Unlock()
	if should {
		s.persist(snap)
	}
}

// Flush força a gravação imediata do estado (ex.: no shutdown).
func (s *State) Flush() {
	s.mu.Lock()
	snap := s.snap
	s.lastPersist = time.Now()
	s.mu.Unlock()
	s.persist(snap)
}

func (s *State) persist(snap Snapshot) {
	snap.Now = time.Now()
	data, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		return
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.path), "state.json.tmp-*")
	if err != nil {
		return
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return
	}
	if err := os.Rename(tmpName, s.path); err != nil {
		_ = os.Remove(tmpName)
	}
}

// --- Setters de alto nível usados pelo coletor ---

func (s *State) SetEndpoints(deviceBase, theraBase string) {
	s.mutate(func(snap *Snapshot) { snap.DeviceBase = deviceBase; snap.TheraBase = theraBase })
}

// SetPoll informa o intervalo de coleta (o painel mostra "próxima coleta em").
func (s *State) SetPoll(seconds int) {
	s.mutate(func(snap *Snapshot) { snap.PollSeconds = seconds })
}

func (s *State) DeviceOK() {
	s.mutate(func(snap *Snapshot) {
		snap.DeviceOnline = true
		snap.DeviceLastOK = time.Now()
		snap.DeviceLastErr = ""
	})
}

func (s *State) DeviceFail(err string) {
	s.mutate(func(snap *Snapshot) { snap.DeviceOnline = false; snap.DeviceLastErr = err })
}

func (s *State) TheraOK() {
	s.mutate(func(snap *Snapshot) { snap.TheraOnline = true; snap.TheraLastOK = time.Now(); snap.TheraLastErr = "" })
}

func (s *State) TheraFail(err string) {
	s.mutate(func(snap *Snapshot) { snap.TheraOnline = false; snap.TheraLastErr = err })
}

func (s *State) Tick() {
	s.mutate(func(snap *Snapshot) { snap.LastTickAt = time.Now() })
}

// CollectNowRequested marca um pedido de coleta imediata (feedback no painel).
func (s *State) CollectNowRequested() {
	s.mutate(func(snap *Snapshot) { snap.CollectNowAt = time.Now() })
}

func (s *State) SetCursor(cursor int64) {
	s.mutate(func(snap *Snapshot) {
		snap.Cursor = cursor
		if cursor > snap.MaxDeviceID {
			snap.MaxDeviceID = cursor
		}
	})
}

func (s *State) SeenDeviceID(id int64) {
	s.mutate(func(snap *Snapshot) {
		if id > snap.MaxDeviceID {
			snap.MaxDeviceID = id
		}
	})
}

// Forwarded registra batidas enviadas e aceitas (incrementa totais e marca sync).
func (s *State) Forwarded(n int) {
	if n <= 0 {
		return
	}
	s.mutate(func(snap *Snapshot) {
		snap.ForwardedTotal += int64(n)
		snap.LastSyncAt = time.Now()
	})
}

// RecordPunch registra UMA batida encaminhada em tempo real: entra no feed e,
// se o evento é de hoje (data local), conta em "batidas hoje".
func (s *State) RecordPunch(p Punch) {
	today := s.today()
	s.mutate(func(snap *Snapshot) {
		if snap.TodayDate != today {
			snap.TodayDate = today
			snap.ForwardedToday = 0
		}
		if p.Time > 0 && time.Unix(normalizeEpoch(p.Time), 0).In(time.Local).Format("2006-01-02") == today {
			snap.ForwardedToday++
		}
		if p.Registration == "" {
			snap.NoRegistration++
		}
		if p.SentAt.IsZero() {
			p.SentAt = time.Now()
		}
		recent := make([]Punch, 0, recentMax)
		recent = append(recent, p)
		for _, old := range snap.RecentPunches {
			if len(recent) >= recentMax {
				break
			}
			recent = append(recent, old)
		}
		snap.RecentPunches = recent
	})
}

// normalizeEpoch aceita epoch em segundos ou milissegundos.
func normalizeEpoch(n int64) int64 {
	if n > 1_000_000_000_000 {
		return n / 1000
	}
	return n
}

func (s *State) SkippedNoTime(n int) {
	if n <= 0 {
		return
	}
	s.mutate(func(snap *Snapshot) { snap.SkippedNoTime += int64(n) })
}

func (s *State) RecordError(err string) {
	s.mutate(func(snap *Snapshot) { snap.ErrorCount++; snap.LastError = err; snap.LastErrorAt = time.Now() })
}

func (s *State) UserSync(observed int) {
	s.mutate(func(snap *Snapshot) {
		snap.LastUserSyncAt = time.Now()
		snap.UsersObserved = observed
		snap.UserSyncError = ""
	})
}

// UserSyncFail registra a falha da sincronização de cadastros/faces (as batidas
// continuam indo; não é "Thera fora").
func (s *State) UserSyncFail(err string) {
	s.mutate(func(snap *Snapshot) { snap.UserSyncError = err; snap.UserSyncErrorAt = time.Now() })
}

// --- Refill (full-sync) ---

// FullSyncInfo descreve o refill que está começando.
type FullSyncInfo struct {
	Source   string
	StartID  int64
	FromUnix int64
	ToUnix   int64
}

// FullSyncStart marca o início de um refill. A estimativa de fim (TargetID) é
// o maior id já visto no aparelho — o painel usa isso para a barra de progresso.
func (s *State) FullSyncStart(info ...FullSyncInfo) {
	var in FullSyncInfo
	if len(info) > 0 {
		in = info[0]
	}
	s.mutate(func(snap *Snapshot) {
		snap.FullSyncRunning = true
		snap.FullSyncStartedAt = time.Now()
		snap.FullSyncSource = in.Source
		snap.FullSyncStartID = in.StartID
		snap.FullSyncFromUnix = in.FromUnix
		snap.FullSyncToUnix = in.ToUnix
		snap.FullSyncTargetID = snap.MaxDeviceID
		snap.FullSyncCurrentID = in.StartID
		snap.FullSyncForwarded = 0
		snap.FullSyncReadTotal = 0
		snap.FullSyncLastResult = ""
	})
	s.Flush()
}

func (s *State) FullSyncProgress(currentID, forwarded, read int64) {
	s.mutate(func(snap *Snapshot) {
		snap.FullSyncCurrentID = currentID
		snap.FullSyncForwarded = forwarded
		snap.FullSyncReadTotal = read
		if currentID > snap.MaxDeviceID {
			snap.MaxDeviceID = currentID
		}
		if currentID > snap.FullSyncTargetID {
			snap.FullSyncTargetID = currentID
		}
	})
}

// FullSyncDone encerra o refill com um resumo legível e o record completo.
func (s *State) FullSyncDone(result string, rec ...audit.Record) {
	s.mutate(func(snap *Snapshot) {
		snap.FullSyncRunning = false
		snap.FullSyncLastResult = result
		snap.FullSyncLastAt = time.Now()
		if len(rec) > 0 {
			r := rec[0]
			snap.FullSyncLast = &r
		}
	})
	s.Flush()
}
