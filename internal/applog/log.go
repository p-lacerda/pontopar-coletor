// Package applog fornece um logger simples com timestamp ISO-8601, que escreve
// simultaneamente num arquivo ao lado do executável (com rotação por tamanho) e,
// opcionalmente, no stdout (modo interativo/run).
//
// O logger é seguro para uso concorrente (protegido por mutex). É deliberadamente
// minimalista: nada de dependências externas, para manter o núcleo do coletor
// como Go puro e o binário pequeno.
package applog

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// maxLogBytes é o tamanho máximo do arquivo de log antes de rotacionar.
// Ao atingir esse limite, o arquivo atual vira ".log.1" (sobrescrevendo o
// anterior) e um novo arquivo é iniciado. Mantemos apenas 1 arquivo antigo
// para não crescer indefinidamente numa PC de cliente.
const maxLogBytes = 5 * 1024 * 1024 // 5 MiB

// Logger é o logger da aplicação.
type Logger struct {
	mu       sync.Mutex
	file     *os.File
	filePath string
	written  int64
	stdout   bool
}

// pkgLogger é o logger de pacote usado pelas funções de conveniência (Infof/Errorf).
// Começa escrevendo só no stderr até Init ser chamado.
var pkgLogger = &Logger{stdout: true}

// New cria um Logger que escreve em <path>. Se stdout for true, também espelha
// as linhas no os.Stdout (útil no modo "run" interativo). Se o arquivo não puder
// ser aberto, o logger continua funcionando apenas com stdout (best-effort) e o
// erro é reportado no retorno.
func New(path string, stdout bool) (*Logger, error) {
	l := &Logger{filePath: path, stdout: stdout}
	if err := l.open(); err != nil {
		// Não falha fatal: ainda conseguimos logar no stdout/stderr.
		return l, err
	}
	return l, nil
}

// Init configura o logger de pacote (usado pelas funções Infof/Errorf globais).
// Deve ser chamado uma vez no arranque. Retorna o Logger criado.
func Init(path string, stdout bool) *Logger {
	l, err := New(path, stdout)
	pkgLogger = l
	if err != nil {
		l.Errorf("aviso: nao foi possivel abrir arquivo de log %q: %v (seguindo so com console)", path, err)
	}
	return l
}

// DefaultPath devolve o caminho padrão do arquivo de log, ao lado do executável.
func DefaultPath(dir string) string {
	return filepath.Join(dir, "pontopar-coletor.log")
}

func (l *Logger) open() error {
	f, err := os.OpenFile(l.filePath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	if fi, statErr := f.Stat(); statErr == nil {
		l.written = fi.Size()
	}
	l.file = f
	return nil
}

// rotate roda o arquivo atual para ".1" quando ultrapassa maxLogBytes.
// Chamado com o mutex já travado.
func (l *Logger) rotate() {
	if l.file == nil {
		return
	}
	_ = l.file.Close()
	old := l.filePath + ".1"
	_ = os.Remove(old)             // remove rotação anterior (ignora erro)
	_ = os.Rename(l.filePath, old) // best-effort
	l.written = 0
	if err := l.open(); err != nil {
		// Se reabrir falhar, desabilita o arquivo e segue só no console.
		l.file = nil
	}
}

func (l *Logger) writeLine(prefix, format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	line := fmt.Sprintf("%s %s%s\n", time.Now().Format(time.RFC3339), prefix, msg)

	l.mu.Lock()
	defer l.mu.Unlock()

	if l.stdout {
		var w io.Writer = os.Stdout
		if prefix == "ERRO " {
			w = os.Stderr
		}
		fmt.Fprint(w, line)
	}
	if l.file != nil {
		if l.written+int64(len(line)) > maxLogBytes {
			l.rotate()
		}
		if l.file != nil { // pode ter sido zerado por rotate falho
			n, _ := l.file.WriteString(line)
			l.written += int64(n)
		}
	}
}

// Infof registra uma linha informativa.
func (l *Logger) Infof(format string, args ...any) { l.writeLine("", format, args...) }

// Errorf registra uma linha de erro.
func (l *Logger) Errorf(format string, args ...any) { l.writeLine("ERRO ", format, args...) }

// Close fecha o arquivo subjacente.
func (l *Logger) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file != nil {
		err := l.file.Close()
		l.file = nil
		return err
	}
	return nil
}

// Funções de conveniência de pacote, encaminham para o logger configurado por Init.

// Infof registra uma linha informativa no logger de pacote.
func Infof(format string, args ...any) { pkgLogger.Infof(format, args...) }

// Errorf registra uma linha de erro no logger de pacote.
func Errorf(format string, args ...any) { pkgLogger.Errorf(format, args...) }
