// Package updater implementa o auto-update via GitHub Releases usando
// creativeprojects/go-selfupdate (que por baixo usa minio/selfupdate para o
// replace atômico do exe no Windows).
//
// Fluxo: detecta a release "latest" para o GOOS/GOARCH atuais, compara semver
// com a versão embutida (main.version via ldflags), e se houver uma versão
// maior, baixa + valida o SHA256 (contra checksums.txt) + substitui o próprio
// executável. O auto-update é BEST-EFFORT: se falhar, apenas loga e o coletor
// continua rodando na versão atual.
//
// RESTART COMO SERVIÇO: o update NÃO reinicia o processo — depois de trocar o
// exe no disco, o binário em memória ainda é o velho. Quando rodando como
// serviço do Windows com recovery=ServiceRestart configurado no install, a
// forma limpa de aplicar a nova versão é sair com código != 0 (ver
// RestartForUpdate): o SCM interpreta como falha e dispara a RecoveryAction que
// relança o novo .exe já gravado. Sair com 0 seria "parada limpa" e o SCM NÃO
// reiniciaria. No modo interativo (run), apenas avisamos e não reiniciamos.
package updater

import (
	"context"
	"errors"
	"fmt"
	"os"
	"regexp"

	"github.com/creativeprojects/go-selfupdate"
	"github.com/p-lacerda/pontopar-coletor/internal/config"
)

// semverRe valida uma versão semver (com "v" opcional). Aceita as formas
// MAJOR.MINOR.PATCH com pré-release/build metadata opcionais (subconjunto
// pragmático da spec semver.org, suficiente para tags de release do GitHub).
// Builds SEM ldflags trazem currentVersion="dev" (ou vazio), que NÃO casa aqui
// e, portanto, DESLIGA o auto-update em vez de estourar no primeiro check.
var semverRe = regexp.MustCompile(`^v?\d+\.\d+\.\d+(?:-[0-9A-Za-z.-]+)?(?:\+[0-9A-Za-z.-]+)?$`)

// isValidSemver diz se s é uma versão semver comparável pelo go-selfupdate.
func isValidSemver(s string) bool { return semverRe.MatchString(s) }

// Logger é a interface mínima de log.
type Logger interface {
	Infof(format string, args ...any)
	Errorf(format string, args ...any)
}

// exitFunc é indireto para permitir teste; em produção é os.Exit.
var exitFunc = os.Exit

// Updater encapsula a checagem/aplicação de updates.
type Updater struct {
	repo    string // "owner/name"
	token   string // opcional (repo privado)
	current string // versão atual (main.version)
	log     Logger
}

// New cria um Updater a partir da config de update. Devolve (nil, nil) —
// auto-update DESLIGADO, no-op — quando:
//   - o repo está vazio (auto-update não configurado), ou
//   - currentVersion NÃO é semver válido (ex.: "dev" num build sem ldflags):
//     nesse caso o go-selfupdate não consegue comparar a versão e estouraria no
//     primeiro check. Como o auto-update é best-effort, ele JAMAIS pode derrubar
//     a coleta — então apenas logamos e desligamos.
//
// O chamador (collector) trata *Updater nil como "não agenda checagens", então
// devolver nil aqui é o desligamento seguro.
func New(cfg *config.Config, currentVersion string, log Logger) (*Updater, error) {
	if cfg.Update.Repo == "" {
		return nil, nil
	}
	if !isValidSemver(currentVersion) {
		log.Errorf("auto-update desligado: versao atual %q nao e semver valido (build sem ldflags?); a coleta segue normal", currentVersion)
		return nil, nil
	}
	return &Updater{
		repo:    cfg.Update.Repo,
		token:   cfg.Update.Token,
		current: currentVersion,
		log:     log,
	}, nil
}

// buildUpdater monta o *selfupdate.Updater configurado com source do GitHub e
// validação por checksums.txt.
func (u *Updater) buildUpdater() (*selfupdate.Updater, selfupdate.Repository, error) {
	source, err := selfupdate.NewGitHubSource(selfupdate.GitHubConfig{
		// Token opcional; se vazio, repo público. Setado, repo privado.
		// IMPORTANTE: o serviço roda como LocalSystem e não herda GITHUB_TOKEN
		// do ambiente, então o token vem SEMPRE da config (não do env).
		APIToken: u.token,
	})
	if err != nil {
		return nil, nil, err
	}
	su, err := selfupdate.NewUpdater(selfupdate.Config{
		Source:    source,
		Validator: &selfupdate.ChecksumValidator{UniqueFilename: "checksums.txt"},
	})
	if err != nil {
		return nil, nil, err
	}
	return su, selfupdate.ParseSlug(u.repo), nil
}

// CheckAndApply verifica se há uma versão mais nova e, se houver, baixa+valida+
// substitui o executável. Devolve (updated, newVersion, err).
//
// updated=true significa que o exe NO DISCO foi trocado; o processo ainda roda
// a versão antiga e precisa ser reiniciado (ver RestartForUpdate).
func (u *Updater) CheckAndApply(ctx context.Context) (updated bool, newVersion string, err error) {
	su, slug, err := u.buildUpdater()
	if err != nil {
		return false, "", err
	}

	latest, found, err := su.DetectLatest(ctx, slug)
	if err != nil {
		if errors.Is(err, selfupdate.ErrValidationAssetNotFound) {
			return false, "", fmt.Errorf("release sem checksums.txt: %w", err)
		}
		if errors.Is(err, selfupdate.ErrChecksumValidationFailed) {
			return false, "", fmt.Errorf("download corrompido (checksum): %w", err)
		}
		return false, "", err
	}
	if !found || latest == nil {
		return false, "", nil // nenhuma release encontrada para este OS/arch
	}
	if latest.LessOrEqual(u.current) {
		return false, "", nil // já está atualizado
	}

	// Baixa + valida sha256 + substitui o exe em execução.
	rel, err := su.UpdateSelf(ctx, u.current, slug)
	if err != nil {
		if errors.Is(err, selfupdate.ErrChecksumValidationFailed) {
			return false, "", fmt.Errorf("download corrompido (checksum): %w", err)
		}
		return false, "", fmt.Errorf("erro ao atualizar: %w", err)
	}
	ver := ""
	if rel != nil {
		ver = rel.Version()
	} else {
		ver = latest.Version()
	}
	return true, ver, nil
}

// RestartForUpdate encerra o processo de modo que o SCM do Windows reinicie o
// serviço (via RecoveryAction configurada no install), relançando o novo .exe.
//
// Só faz sentido quando rodando COMO SERVIÇO. No modo interativo, o chamador
// não deve invocar isto (deve apenas logar que há uma nova versão no disco e
// pedir reinício manual).
//
// Usa código de saída != 0 de propósito: saída zero = parada limpa (o SCM não
// aciona recovery). O chamador DEVE garantir o flush da fila/estado antes.
func (u *Updater) RestartForUpdate() {
	u.log.Infof("update aplicado no disco; encerrando com codigo !=0 para o SCM reiniciar o novo binario")
	exitFunc(1)
}
