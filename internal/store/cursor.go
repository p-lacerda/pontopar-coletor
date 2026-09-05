// Package store persiste o cursor de leitura (o maior access_log.id já
// confirmado pelo Thera) num arquivo cursor.json ao lado do executável.
//
// A escrita é ATÔMICA (grava num arquivo temporário, faz fsync, e renomeia):
// isso garante que um crash no meio da gravação nunca deixe o cursor.json
// truncado/corrompido. Um cursor corrompido faria getCursor cair para 0 e
// reprocessar TODO o histórico do aparelho — tolerável (o Thera deduplica),
// mas caro; o temp+fsync+rename evita isso.
package store

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// cursorFile é o nome do arquivo de cursor.
const cursorFile = "cursor.json"

type cursorData struct {
	LastId int64 `json:"lastId"`
}

// Cursor guarda e persiste o último id confirmado. Seguro para uso concorrente.
type Cursor struct {
	mu   sync.Mutex
	dir  string
	path string
}

// NewCursor cria um cursor persistido em <dir>/cursor.json.
func NewCursor(dir string) *Cursor {
	return &Cursor{dir: dir, path: filepath.Join(dir, cursorFile)}
}

// Get lê o cursor do disco. Se o arquivo não existir ou estiver corrompido,
// devolve 0 (espelha o try/catch->0 do Node).
func (c *Cursor) Get() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	data, err := os.ReadFile(c.path)
	if err != nil {
		return 0
	}
	var cd cursorData
	if err := json.Unmarshal(data, &cd); err != nil {
		return 0
	}
	return cd.LastId
}

// Set grava o cursor de forma ATÔMICA (temp + fsync + rename).
func (c *Cursor) Set(id int64) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	data, err := json.Marshal(cursorData{LastId: id})
	if err != nil {
		return err
	}

	// Arquivo temporário no MESMO diretório (rename atômico exige mesmo FS).
	tmp, err := os.CreateTemp(c.dir, cursorFile+".tmp-*")
	if err != nil {
		return fmt.Errorf("criar temp do cursor: %w", err)
	}
	tmpName := tmp.Name()
	// Em caso de erro no meio do caminho, garantir remoção do temp.
	cleanup := func() {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
	}

	if _, err := tmp.Write(data); err != nil {
		cleanup()
		return fmt.Errorf("escrever temp do cursor: %w", err)
	}
	// fsync: garante que os bytes chegaram ao disco antes do rename.
	if err := tmp.Sync(); err != nil {
		cleanup()
		return fmt.Errorf("fsync do cursor: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("fechar temp do cursor: %w", err)
	}

	// Rename atômico sobre o arquivo final.
	if err := os.Rename(tmpName, c.path); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("rename do cursor: %w", err)
	}

	// Best-effort: fsync do diretório pai para durabilizar a entrada de diretório
	// (no Windows abrir diretório para sync não é suportado; ignoramos o erro).
	if d, derr := os.Open(c.dir); derr == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}
