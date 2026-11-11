//go:build !windows

package desktop

import (
	"errors"
	"fmt"
	"os"
)

// Run existe para o projeto compilar fora do Windows. O app é exclusivo do Windows
// (para ver o painel no Linux use: go run ./cmd/pontopar-preview).
func Run(version string, minimized bool) error {
	return errors.New("o app da bandeja só existe no Windows")
}

// ShowStartupError imprime o erro no stderr.
func ShowStartupError(err error) { fmt.Fprintln(os.Stderr, "PontoPar:", err) }

// LogStartupError imprime o erro no stderr.
func LogStartupError(err error) { fmt.Fprintln(os.Stderr, "PontoPar:", err) }
