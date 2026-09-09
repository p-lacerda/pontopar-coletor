//go:build !windows

package setup

import (
	"errors"
	"path/filepath"

	"github.com/p-lacerda/pontopar-coletor/internal/config"
)

var errNotWindows = errors.New("assistente de instalacao so e suportado no Windows")

func InstallDir() string { return "" }

func InstalledExePath() string { return "" }

func ActiveConfigDir() (string, error) { return config.ExeDir() }

func Install() error { return errNotWindows }

func Uninstall() error { return errNotWindows }

func ShowResult(title, message string, isError bool) {}

func samePath(a, b string) bool { return filepath.Clean(a) == filepath.Clean(b) }
