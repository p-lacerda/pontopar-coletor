# Coletor PontoPar (Control iD iDFace → Thera)

Programa Windows (em Go) que roda numa PC leve **na mesma rede (LAN/WiFi)** do
aparelho **Control iD iDFace** (linha de Acesso, HTTP porta 80). Faz *polling*
das batidas faciais do aparelho e encaminha cada batida NOVA para o **Thera** na
nuvem. Instala-se por um **assistente do Windows** (auto-start no boot), traz uma
**interface gráfica com ícone na bandeja** e tem **reprocessamento offline**
(não perde batida se a PC ou o Thera ficarem
offline) e **auto-update** via GitHub Releases.

- Fala **HTTP** com o iDFace (porta 80, sem TLS) — só na LAN.
- Fala **HTTPS** com o Thera (nuvem) — precisa apenas de internet de saída.
- **Idempotente:** o Thera deduplica por `(aparelho, id da batida, hora)`; o
  cursor só avança depois que o Thera confirma (200). Se o Thera cair, o cursor
  não avança e reprocessa depois — nada se perde.
- **Painel web local** em `http://127.0.0.1:47114` (só loopback): status ao vivo,
  log, auditoria, **full-sync** ("Consultar o Control iD completo") e todas as
  configurações. Abra pela bandeja → **Abrir painel completo**.
- **Trava anti-colapso de horário:** o coletor **NUNCA** envia uma batida sem o
  `time` real do aparelho. O `punchedAt` e a chave de dedupe do Thera derivam do
  `time`; uma batida sem `time` viraria um carimbo "agora" e colapsaria vários
  eventos num segundo só (o bug histórico). Batidas sem `time` são **recusadas** e
  **auditadas**, nunca carimbadas.
- **Auditoria** persistente (`audit.jsonl`, exportável em CSV): cada
  sincronização registra a faixa de ids, quantas batidas foram lidas/enviadas/
  recusadas e se a drenagem foi **verificada** como completa — a prova de completude.

---

## 1. Como funciona (visão geral)

A cada `pollSeconds`:

1. Garante sessão no iDFace (relogin preguiçoso se a sessão expirou).
2. Busca em `access_logs` as batidas com `event=7` (acesso concedido) e
   `id > cursor`, ordenadas por `id` (limite 500).
3. **Traduz `user_id` → matrícula (`registration`)** consultando os `users` do
   aparelho, com cache em memória. O `access_log` traz o `user_id` *interno* do
   aparelho (um contador), **não a matrícula**; o Thera identifica o colaborador
   pela **matrícula**.
4. Para cada batida (em ordem de `id`): monta o payload no formato do webhook
   `/dao` e faz `POST` no Thera. **Só se o Thera responder 200** o cursor avança
   (salvo atomicamente em `cursor.json`).

### Garantia de não-perda (invariantes)

- **Thera fora / resposta não-2xx:** o `POST` falha → o ciclo aborta ANTES de
  avançar o cursor → a mesma batida (e as seguintes) são reprocessadas no
  próximo ciclo. **O cursor nunca avança sem um 200.**
- **Aparelho fora / erro de rede:** o ciclo erra, o loop dorme e tenta de novo;
  a sessão é zerada para forçar relogin. Cursor intacto.
- **Crash no meio do ciclo:** pior caso = uma batida foi aceita pelo Thera mas o
  processo morreu antes de salvar o cursor. Ao reiniciar, o cursor está no valor
  anterior → reenvia → o Thera deduplica. **Zero perda**, custo = 1 reenvio
  idempotente.
- **Cursor atômico:** `cursor.json` é gravado com *temp + fsync + rename*, então
  um crash durante a escrita nunca deixa o arquivo corrompido (o que faria
  reprocessar todo o histórico).

> A tradução da matrícula, o NSR, a assinatura e o AFD continuam sendo do Thera
> — o iDFace é só o leitor.

### Painel web local, full-sync e auditoria

- **Painel** (`http://127.0.0.1:47114`, só loopback — ligado por default): roda
  **dentro do serviço**, então mostra o estado real do coletor. Tem cartões de
  status (conexão com o aparelho e com o Thera, cursor, batidas enviadas, batidas
  recusadas por falta de horário, erros), **log ao vivo**, **tabela de auditoria**
  (com exportação CSV), o formulário com **todas** as configurações e os botões de
  ação. Mutações exigem um token injetado na página (um site no navegador não
  consegue lê-lo). Abra pela bandeja do coletor → **Abrir painel completo**.
- **Full-sync ("Consultar o Control iD completo"):** um botão que varre **TODO**
  o `access_logs` concedido do aparelho (paginado por `id`, do mais antigo ao mais
  novo), **sempre carregando o `time` real** de cada evento, e reenvia tudo ao
  Thera. É **idempotente** (o Thera deduplica por device+id+hora), **retomável**
  (cursor próprio em `fullsync.json`) e **verificado** ao final (uma releitura
  acima do cursor tem que vir vazia). Use quando precisar recuperar o histórico de
  forma confiável, sem receber "por partes". Enquanto roda, o polling incremental
  é pausado (um único remetente por vez). Detecta **rotação do buffer** (lacuna) e
  a registra na auditoria.
- **Auditoria (`audit.jsonl`):** cada run (incremental ou full) vira uma linha com
  `fromId → toId`, `read/forwarded/skippedNoTime/skippedBadId`, `rotatedGap`,
  `verified` e duração. Exporte em CSV pelo painel (`/api/audit.csv`) para anexar a
  uma prova de completude.
- **Iniciar com o Windows:** um checkbox (na janela e no painel) alterna o
  *StartType* do serviço entre **Automático** (sobe no boot) e **Manual**. Pelo
  painel (que roda como o serviço) a mudança não pede UAC.

---

## 2. Configuração (`config.json`)

Copie `config.example.json` para **`config.json`** e coloque-o **ao lado do
`pontopar-coletor.exe`** (o serviço não usa o diretório de trabalho; como
serviço, o cwd é `C:\Windows\System32`, por isso tudo — config, cursor e log —
fica ao lado do executável).

| Campo | Descrição |
|---|---|
| `deviceIp` | IP do aparelho na LAN (obrigatório). |
| `devicePort` | Porta HTTP do aparelho. Default `80`. |
| `login` / `password` | Credenciais do aparelho (fábrica: `admin`/`admin`). |
| `deviceId` | ID do device no Thera, usado como fallback quando o `access_log` não traz `device_id`. Aceita número ou string; `""` se não souber. |
| `theraBase` | URL base da API do Thera. |
| `deviceSecret` | Segredo que o Thera gera ao cadastrar o aparelho (vai na URL do `/dao`). Obrigatório. |
| `pollSeconds` | Intervalo de verificação. Default `15`. |
| `batchSize` | Tamanho da página lida do aparelho por vez. Default `500` (1–1000). |
| `maxRetries` | Retentativas por batida no envio ao Thera antes de desistir no ciclo. Default `4`. |
| `retryBackoffSeconds` | Base do backoff exponencial entre retentativas (2s, 4s, 8s… até 30s). Default `2`. |
| `registrationOverrides` | Mapa `{"user_id":"matrícula"}` com **prioridade** sobre o cadastro do aparelho (conserta usuário sem `registration`). Opcional. |
| `ui.enabled` | Liga/desliga o painel web local. Default `true`. |
| `ui.port` | Porta do painel em `127.0.0.1`. Default `47114`. |
| `ui.bind` | Endereço de escuta — **sempre loopback** por segurança. Default `127.0.0.1`. |
| `update.repo` | `owner/name` do repositório das releases. **Vazio desliga o auto-update.** |
| `update.checkHours` | Intervalo entre checagens de update. Default `6`. |
| `update.token` | Token do GitHub (só para repositório **privado** — ver nota abaixo). |

> Todos os campos novos (`batchSize`, `maxRetries`, `ui`, …) têm defaults seguros:
> um `config.json` antigo continua funcionando sem alteração.

> **IMPORTANTE:** cadastre a **matrícula** de cada colaborador no campo
> `registration` do usuário no aparelho, **igual** à matrícula dele no Thera
> (mesma grafia, inclusive zeros à esquerda). É esse campo que liga a batida ao
> colaborador. Se um usuário do aparelho estiver sem `registration`, a batida
> vai sem o campo e o Thera cai no mapeamento manual (`ControlIdUserMap`) — se
> não houver, a batida é ignorada e aparece no log do Thera.

---

## 3. Instalar pelo PontoPar Setup (recomendado)

Baixe e dê dois cliques em `PontoPar-Setup_vX.Y.Z_windows_amd64.exe` na seção
**Releases**. Ele é um instalador separado — não é o programa coletor.

1. Clique em **Configurações do Control iD e Thera**.
2. O IP da Show de Bola (`192.168.0.140`) já vem preenchido; confira os dados,
   cole o **Segredo do Thera** no campo obrigatório e clique em **Salvar
   configurações**. A chave é salva apenas nesta PC, em
   `C:\ProgramData\PontoParColetor\config.json`.
3. De volta ao Setup, clique em **Instalar e iniciar**. O Windows pede
   autorização de administrador nessa etapa.

O setup copia o coletor para `C:\ProgramData\PontoParColetor\`, registra o
serviço de início automático e cria a entrada **PontoPar Coletor** em
**Configurações → Aplicativos instalados**. A desinstalação por essa tela ou
pelo próprio Setup para a coleta e remove o serviço, preservando configuração,
cursor e logs para não apagar registros de ponto sem querer.

> A release pública não carrega senha ou `deviceSecret` de cliente. Para uma
> instalação já preenchida, gere e entregue um Setup privado do cliente.

## 4. Tela do coletor e início automático

Dê dois cliques em `pontopar-coletor.exe`. A tela permite conferir ou alterar
IP, porta e credenciais do Control iD, salvar a configuração e testar se a PC
consegue alcançar o aparelho. O endereço do Thera, o segredo e o repositório
de atualizações já vêm no `config.json` e não são expostos na tela.

- Ao fechar a janela, ele **não para a coleta**: apenas fica minimizado no
  ícone do PontoPar perto do relógio do Windows.
- Clique no ícone da bandeja para abrir a tela novamente. O menu também tem
  `Fechar janela`, que fecha só a tela; o serviço instalado continua rodando.
- Clique em **Assistente: instalar / desinstalar** e depois em **Instalar e
  iniciar**. O Windows mostrará a confirmação de administrador (UAC); depois
  de aceitar, o programa é copiado para
  `C:\ProgramData\PontoParColetor\`, iniciado e configurado para iniciar a
  cada boot.
- O instalador não depende da pasta Downloads: ela pode ser movida ou limpa
  depois da instalação sem parar a coleta.
- No mesmo assistente, **Desinstalar serviço** para a coleta e remove o início
  automático. Configuração, cursor e logs são preservados para não apagar
  registros de ponto acidentalmente.

O coletor precisa ser instalado como serviço apenas para operar sozinho 24h:
continuar após reiniciar a PC, mesmo sem usuário logado, e receber reinício
automático do Windows em caso de falha ou atualização. Para só testar a tela,
não é necessário instalar.

## 5. Instalar como serviço do Windows (alternativa por linha de comando)

Baixe o `pontopar-coletor.exe` mais recente (das Releases) e coloque numa pasta
fixa, ex.: `C:\PontoPar\`. Ao lado dele, crie o `config.json`.

Abra o **Prompt de Comando como Administrador** (o SCM exige elevação) e rode:

```bat
cd C:\PontoPar
pontopar-coletor.exe install
pontopar-coletor.exe start
```

- `install` registra o serviço com **StartType=Automatic** (sobe no boot) e
  **recovery=Restart** (reinicia nas primeiras falhas; contador reseta em 24h).
- Verificar estado: `pontopar-coletor.exe status`
- Parar: `pontopar-coletor.exe stop`
- Remover: `pontopar-coletor.exe uninstall`

### Testar sem instalar (foreground)

```bat
pontopar-coletor.exe run
```

Roda em primeiro plano e **espelha o log no console** (além do arquivo). Deve
aparecer `login OK no iDFace` e, a cada batida facial, `batida encaminhada:
id=...`.

### Logs

- Arquivo: `pontopar-coletor.log` (ao lado do exe), com rotação simples (roda
  para `.log.1` ao passar de ~5 MiB).
- Eventos do serviço (start/stop/erros do SCM): **Visualizador de Eventos do
  Windows**, fonte `PontoParColetor`.

---

## 6. Auto-update (GitHub Releases)

O coletor checa a release `latest` do repositório em `update.repo` a cada
`update.checkHours`. Se houver uma versão **maior** (semver) que a embutida:

1. Baixa o asset do Windows (`...windows_amd64.exe`).
2. **Valida o SHA256** contra o `checksums.txt` da release.
3. Substitui o próprio `.exe` no disco (rename atômico; o Windows esconde o
   `.old` que sobra — pode ser apagado no próximo boot).
4. **Reinicia** para carregar a nova versão.

### Como o restart funciona como serviço

O update **não reinicia** o processo sozinho — depois de trocar o `.exe`, o
binário em memória ainda é o antigo. Quando rodando como serviço, o coletor
encerra com **código de saída ≠ 0**; o SCM interpreta como falha e dispara a
`RecoveryAction{Restart}` configurada no `install`, relançando o novo `.exe` já
gravado. (Sair com `0` seria "parada limpa" e o SCM **não** reiniciaria.) No
modo `run` (foreground), o coletor apenas avisa no log que há uma nova versão e
pede reinício manual.

### Repositório privado

O serviço roda como **LocalSystem** e **não herda** a variável de ambiente
`GITHUB_TOKEN`. Por isso, para repositório privado, coloque o token em
`update.token` no `config.json` (não confie no ambiente).

### Layout de release esperado

Cada release precisa conter:

- `pontopar-coletor_vX.Y.Z_windows_amd64.exe` (o sufixo `windows_amd64.exe` é o
  que o updater casa por OS/arch);
- `PontoPar-Setup_vX.Y.Z_windows_amd64.exe` (instalador com tela e UAC);
- `checksums.txt` (formato `sha256sum`: `<hash>  <nome-do-arquivo>`), listando
  exatamente o nome do `.exe` publicado.

O workflow `.github/workflows/release.yml` gera isso automaticamente ao empurrar
uma tag `v*`.

---

## 7. Build (desenvolvimento)

Go 1.24+ (o alvo é `windows/amd64`; compila no Linux — é Go puro +
`golang.org/x/sys/windows`).

```sh
# compilar o .exe com a versão embutida
GOOS=windows GOARCH=amd64 go build -ldflags "-H windowsgui -X main.version=v1.2.3" \
  -o pontopar-coletor.exe ./cmd/pontopar-coletor

# verificação
GOOS=windows GOARCH=amd64 go vet ./...
```

### Lançar uma versão

```sh
git tag v1.2.3
git push origin v1.2.3    # dispara .github/workflows/release.yml
```

---

## 8. Estrutura do projeto

```
cmd/pontopar-coletor/main.go   interface gráfica e CLI de serviço
cmd/pontopar-coletor/app.manifest + rsrc_windows_amd64.syso
                                manifesto embutido dos controles visuais Windows
cmd/pontopar-installer          PontoPar-Setup: instalador/desinstalador Windows
internal/app                   supervisor: carrega config, sobe o painel, roda o coletor, recarga a quente
internal/gui                   janela walk + bandeja + botão "Abrir painel" + checkbox "Iniciar com o Windows"
internal/webui                 painel web local (localhost): status, log, auditoria, full-sync, config
internal/setup                 assistente de instalação/desinstalação (UAC)
internal/config                carrega/valida config.json (ao lado do exe); batch/retry/overrides/ui
internal/idface                cliente do iDFace (login, sessão, access_logs paginado, de-para, min id)
internal/thera                 cliente do webhook /dao do Thera
internal/collector             loop de coleta, trava anti-colapso de horário, full-sync, cursor pós-200
internal/state                 estado vivo (status/contadores) + state.json
internal/audit                 auditoria persistente (audit.jsonl) + export CSV — prova de completude
internal/store                 cursor.json / fullsync.json com escrita atômica (temp+fsync+rename)
internal/updater               auto-update via GitHub Releases (checksums.txt, self-replace, restart)
internal/winservice            serviço do Windows (SCM): install/start/stop/status + auto-start toggle
internal/applog                log em arquivo (rotativo) + console no modo run
.github/workflows/release.yml  build windows/amd64 + checksums + GitHub Release em tag v*
```

---

## 9. Troubleshooting

- **`install` diz "conectar ao SCM (rode como Administrador)":** abra o Prompt
  **como Administrador**. `install`/`uninstall`/`start`/`stop` exigem elevação.
- **`config.json incompleto`:** faltam `deviceIp`, `login`, `theraBase` ou
  `deviceSecret`. Confira o arquivo ao lado do exe.
- **Thera responde 401:** o `deviceSecret` está errado/ausente, ou o device não
  foi cadastrado no Thera. O coletor continua tentando; corrija o segredo.
- **Batidas chegam sem colaborador no Thera:** o `registration` do usuário no
  aparelho está vazio ou diferente da matrícula no Thera. Cadastre igual (com
  zeros à esquerda).
- **`login no aparelho falhou`:** IP/porta/credenciais errados, ou a PC não está
  na mesma rede do aparelho (teste `ping <deviceIp>`).
- **Auto-update não acontece:** confira `update.repo`; para repo privado,
  preencha `update.token`; veja no log a linha `auto-update habilitado`. Falhas
  de update são best-effort (logadas) e **não** derrubam o coletor.
- **Sobrou um arquivo `.old` ao lado do exe:** é o binário antigo após um
  update; o Windows não consegue apagá-lo enquanto o processo roda. Pode remover
  manualmente ou deixar (fica oculto).
```
