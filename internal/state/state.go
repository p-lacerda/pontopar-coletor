// Package state mantém o ESTADO VIVO do coletor (status de conexão, cursor,
// contadores, última sync, progresso do full-sync) num objeto seguro para uso
// concorrente. A interface gráfica e o painel web leem um Snapshot imutável; o
// coletor atualiza os campos conforme opera.
//
// O estado também é persistido (throttle) em state.json ao lado do executável,
// para diagnóstico e para que um processo separado (a janela walk) possa mostrar
// o status mesmo sem falar com o serviço.
package state

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

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

	// Sincronização de usuários/faces (manifesto do Thera).
	LastUserSyncAt time.Time `json:"lastUserSyncAt,omitempty"`
	UsersObserved  int       `json:"usersObserved"`

	// Progresso do full-sync on-demand.
	FullSyncRunning    bool      `json:"fullSyncRunning"`
	FullSyncStartedAt  time.Time `json:"fullSyncStartedAt,omitempty"`
	FullSyncCurrentID  int64     `json:"fullSyncCurrentId"`
	FullSyncForwarded  int64     `json:"fullSyncForwarded"`
	FullSyncReadTotal  int64     `json:"fullSyncReadTotal"`
	FullSyncLastResult string    `json:"fullSyncLastResult,omitempty"`
	FullSyncLastAt     time.Time `json:"fullSyncLastAt,omitempty"`
}

// State é o estado mutável protegido por mutex.
type State struct {
	mu   sync.Mutex
	snap Snapshot

	path         string
	lastPersist  time.Time
	persistEvery time.Duration
}

// New cria o estado. dir é onde state.json será gravado.
func New(dir, version string) *State {
	return &State{
		snap:         Snapshot{Version: version, StartedAt: time.Now()},
		path:         filepath.Join(dir, "state.json"),
		persistEvery: 3 * time.Second,
	}
}

// Get devolve um Snapshot atual (com Now preenchido).
func (s *State) Get() Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	snap := s.snap
	snap.Now = time.Now()
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

// Forwarded registra uma batida enviada e aceita (incrementa totais e marca sync).
func (s *State) Forwarded(n int) {
	if n <= 0 {
		return
	}
	s.mutate(func(snap *Snapshot) {
		snap.ForwardedTotal += int64(n)
		snap.LastSyncAt = time.Now()
	})
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
	s.mutate(func(snap *Snapshot) { snap.LastUserSyncAt = time.Now(); snap.UsersObserved = observed })
}

// --- Full-sync ---

func (s *State) FullSyncStart() {
	s.mutate(func(snap *Snapshot) {
		snap.FullSyncRunning = true
		snap.FullSyncStartedAt = time.Now()
		snap.FullSyncCurrentID = 0
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
	})
}

func (s *State) FullSyncDone(result string) {
	s.mutate(func(snap *Snapshot) {
		snap.FullSyncRunning = false
		snap.FullSyncLastResult = result
		snap.FullSyncLastAt = time.Now()
	})
	s.Flush()
}
