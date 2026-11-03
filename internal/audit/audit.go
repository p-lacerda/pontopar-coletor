// Package audit mantém um registro PERSISTENTE e append-only de cada
// sincronização feita pelo coletor — para o cliente poder PROVAR a completude.
//
// Cada linha do arquivo audit.jsonl descreve um "run" de coleta: a faixa de ids
// varrida no aparelho, quantas batidas foram lidas/enviadas/puladas, se houve
// rotação do buffer (lacuna irreversível) e o resultado da verificação final.
// O formato é JSON Lines (um objeto por linha): fácil de anexar sem reescrever,
// resistente a um processo morto no meio (a última linha parcial é ignorada na
// leitura), e exportável para CSV.
//
// Por que isto importa: o bug que motivou a reescrita foi um replay em massa que
// CARIMBOU dezenas de pessoas no MESMO segundo (perda do `time` real durante o
// reenvio). O audit torna qualquer anomalia dessas visível e auditável: cada run
// diz exatamente quantos eventos foram enviados com `time` real e quantos foram
// RECUSADOS por não ter `time` (nunca enviados com carimbo falso).
package audit

import (
	"bufio"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"
)

// Mode identifica o tipo de run de coleta registrado no audit.
const (
	// ModeIncremental é o polling normal (id > cursor) a cada pollSeconds.
	ModeIncremental = "incremental"
	// ModeFull é a varredura completa on-demand ("Consultar o Control iD
	// completo"): drena TODO o access_logs do aparelho a partir de um id inicial.
	ModeFull = "full"
	// ModeFullPartial marca um run completo interrompido (ctx cancelado / erro) —
	// o progresso fica salvo e é retomável.
	ModeFullPartial = "full-partial"
)

// Record é um run de coleta auditado. Os contadores NUNCA são estimados: são o
// que o coletor realmente leu do aparelho e confirmou (200) no Thera.
type Record struct {
	// Time é o instante em que o run TERMINOU (UTC, RFC3339 na serialização).
	Time time.Time `json:"time"`
	// Mode: incremental | full | full-partial.
	Mode string `json:"mode"`
	// FromID é o cursor inicial (EXCLUSIVO) do run; os ids lidos são > FromID.
	FromID int64 `json:"fromId"`
	// ToID é o maior access_log.id efetivamente enviado neste run (o novo cursor).
	ToID int64 `json:"toId"`
	// DeviceMinID é o menor id de access_log (event=7) ainda presente no aparelho
	// no início do run. Usado para detectar rotação do buffer (ver RotatedGap).
	DeviceMinID int64 `json:"deviceMinId,omitempty"`
	// Read é quantas batidas (event=7) foram LIDAS do aparelho neste run.
	Read int `json:"read"`
	// Forwarded é quantas foram ACEITAS pelo Thera (HTTP 200). Cada uma carregou o
	// `time` REAL do evento no aparelho — nunca um timestamp de processamento.
	Forwarded int `json:"forwarded"`
	// SkippedNoTime é quantas batidas foram RECUSADAS por não ter `time` válido.
	// Esta é a trava anti-corrupção: uma batida sem `time` jamais é enviada (o
	// backend carimbaria "agora" e colapsaria vários eventos no mesmo segundo).
	SkippedNoTime int `json:"skippedNoTime"`
	// SkippedBadID é quantas batidas tinham id ilegível (não dá para avançar o
	// cursor com segurança) e foram puladas.
	SkippedBadID int `json:"skippedBadId"`
	// RotatedGap > 0 indica que o buffer do aparelho ROTACIONOU: há ids entre o
	// cursor inicial e o menor id ainda no aparelho que foram perdidos do device
	// (só recuperáveis no Thera se já tinham sido enviados antes).
	RotatedGap int64 `json:"rotatedGap,omitempty"`
	// Verified=true quando, ao final, uma releitura (id > ToID) voltou vazia —
	// prova de que o aparelho não tem mais nada acima do cursor (drenagem completa).
	Verified bool `json:"verified"`
	// DurationMs é a duração do run em milissegundos.
	DurationMs int64 `json:"durationMs"`
	// Err, se presente, é a mensagem do erro que abortou o run.
	Err string `json:"error,omitempty"`
	// Note é um texto livre opcional (ex.: "disparado pelo painel").
	Note string `json:"note,omitempty"`
}

// fileName é o nome do arquivo de auditoria, ao lado do executável.
const fileName = "audit.jsonl"

// maxBytes é o teto antes de rotacionar o audit para .1 (mantém 1 geração
// anterior). Grande de propósito: a auditoria precisa ser longa para provar
// histórico; 64 MiB cobrem centenas de milhares de runs.
const maxBytes = 64 * 1024 * 1024

// Log anexa records de auditoria a <dir>/audit.jsonl de forma segura para uso
// concorrente.
type Log struct {
	mu      sync.Mutex
	path    string
	written int64
}

// New cria um Log apontando para <dir>/audit.jsonl (o arquivo é criado no
// primeiro Append). Não falha se o diretório não existir ainda; o Append reporta.
func New(dir string) *Log {
	p := filepath.Join(dir, fileName)
	l := &Log{path: p}
	if fi, err := os.Stat(p); err == nil {
		l.written = fi.Size()
	}
	return l
}

// Path devolve o caminho do arquivo de auditoria.
func (l *Log) Path() string { return l.path }

// Append grava um record no fim do arquivo (append + fsync). A escrita de uma
// única linha JSON é atômica o suficiente para JSONL: um crash no meio deixa no
// máximo uma última linha parcial, descartada na leitura.
func (l *Log) Append(r Record) error {
	if r.Time.IsZero() {
		r.Time = time.Now().UTC()
	} else {
		r.Time = r.Time.UTC()
	}
	data, err := json.Marshal(r)
	if err != nil {
		return err
	}
	data = append(data, '\n')

	l.mu.Lock()
	defer l.mu.Unlock()

	if l.written+int64(len(data)) > maxBytes {
		l.rotate()
	}
	f, err := os.OpenFile(l.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o640)
	if err != nil {
		return fmt.Errorf("abrir auditoria: %w", err)
	}
	n, werr := f.Write(data)
	l.written += int64(n)
	if werr != nil {
		_ = f.Close()
		return fmt.Errorf("gravar auditoria: %w", werr)
	}
	// fsync: a prova de completude não pode se perder num corte de energia.
	_ = f.Sync()
	return f.Close()
}

// rotate roda o arquivo atual para .1 (mantém 1 geração). Chamado com mu travado.
func (l *Log) rotate() {
	old := l.path + ".1"
	_ = os.Remove(old)
	_ = os.Rename(l.path, old)
	l.written = 0
}

// Recent devolve os últimos n records (ordem cronológica crescente). n<=0 devolve
// todos. Linhas inválidas (ex.: última linha parcial após crash) são ignoradas.
func (l *Log) Recent(n int) ([]Record, error) {
	all, err := ReadAll(l.path)
	if err != nil {
		return nil, err
	}
	if n > 0 && len(all) > n {
		all = all[len(all)-n:]
	}
	return all, nil
}

// ReadAll lê todos os records de um arquivo JSONL de auditoria. Um arquivo
// ausente devolve lista vazia sem erro (ainda não houve sync).
func ReadAll(path string) ([]Record, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()
	var out []Record
	sc := bufio.NewScanner(f)
	// Linhas de auditoria são pequenas, mas um buffer generoso evita erro em
	// records grandes (ex.: notas longas).
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var r Record
		if err := json.Unmarshal(line, &r); err != nil {
			// Linha parcial/corrompida (crash no meio do append): ignora.
			continue
		}
		out = append(out, r)
	}
	if err := sc.Err(); err != nil {
		return out, err
	}
	return out, nil
}

// ExportCSV escreve os records em CSV (cabeçalho + uma linha por run), para o
// cliente anexar numa prova de completude.
func ExportCSV(w io.Writer, records []Record) error {
	cw := csv.NewWriter(w)
	header := []string{
		"time", "mode", "fromId", "toId", "deviceMinId", "read", "forwarded",
		"skippedNoTime", "skippedBadId", "rotatedGap", "verified", "durationMs", "error", "note",
	}
	if err := cw.Write(header); err != nil {
		return err
	}
	for _, r := range records {
		row := []string{
			r.Time.UTC().Format(time.RFC3339),
			r.Mode,
			strconv.FormatInt(r.FromID, 10),
			strconv.FormatInt(r.ToID, 10),
			strconv.FormatInt(r.DeviceMinID, 10),
			strconv.Itoa(r.Read),
			strconv.Itoa(r.Forwarded),
			strconv.Itoa(r.SkippedNoTime),
			strconv.Itoa(r.SkippedBadID),
			strconv.FormatInt(r.RotatedGap, 10),
			strconv.FormatBool(r.Verified),
			strconv.FormatInt(r.DurationMs, 10),
			r.Err,
			r.Note,
		}
		if err := cw.Write(row); err != nil {
			return err
		}
	}
	cw.Flush()
	return cw.Error()
}
