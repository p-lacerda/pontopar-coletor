//go:build !windows

package autorun

import "errors"

var errNotWindows = errors.New("inicialização automática só existe no Windows")

func Command(exePath string) string { return `"` + exePath + `" tray` }
func Enable(exePath string) error   { return errNotWindows }
func Disable() error                { return errNotWindows }
func Enabled() bool                 { return false }
func Registered() bool              { return false }
