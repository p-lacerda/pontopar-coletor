//go:build !windows

package gui

import "errors"

// Run existe para permitir testes no Linux. A interface é exclusiva do Windows.
func Run(version string) error {
	return errors.New("a interface grafica do coletor so esta disponivel no Windows")
}
