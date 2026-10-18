# Barroso Harness

Harness de agentes em **Go**, feita do zero: um binário, sem base de terceiros, para Windows/macOS/Linux.

- **Execução YOLO permanente**: as tools rodam sem prompts de aprovação, allowlist ou allow/ask/deny. Limites técnicos (timeout, teto de saída, cancelamento) são operacionais — não são permissionamento.
- **Provider configurável** no padrão **OpenAI Chat Completions** (`base_url` + chave + model), streaming SSE com `include_usage`.
- **Sessão interativa** com transcript em memória e comandos `/model`, `/effort`, `/provider`, `/new`, `/resume`, `/save`, `/update`, `/help`, `/exit`. No terminal, a saída usa paleta ANSI-256 e glifos (`▶` tool, `✗` erro, `✓` ok, `◆` guardrail) com status line viva e rodapé de tokens por turno; em pipe/`-p` sai texto cru, sem escapes.
- **`BARROSO.md`** entra como instrução de sistema: do diretório do projeto (e seus pais) e/ou o global em `~/.barroso-harness/BARROSO.md`.
- **Contexto com guardrail**: orçamento por janela do modelo, estimador calibrável pelo `usage` real e compactação proativa/reativa monotônica. O transcript de cada turno só é confirmado em rodada completa.
- **Sessões persistentes** em SQLite append-only (`sessions.db`), com `/resume` e blobs para resultados grandes de tool.
- **Subsistemas**: MCP (stdio e HTTP streamable), skills (`SKILL.md`), `web_fetch`/`web_search`, subagentes async (`task_spawn`/`task_status`/`task_result`, sem recursão de delegação) e **ACP** (`bh -acp`) para editores.
- **Atualização pelo próprio CLI**: `/update` baixa, verifica SHA256 e instala a release mais nova.
- **Instalação e receitas**: https://harness.barroso.tec.br

## Instalar

Binários das releases (5 plataformas + `SHA256SUMS`): a página do site tem os comandos prontos. Ou construir do fonte:

```bash
git clone https://github.com/ruanbarroso/barroso-harness
cd barroso-harness && go build -o barroso-harness ./cmd/barroso-harness
```

## Configurar e usar

No primeiro uso, execute `bh` (ou `barroso-harness`) sem argumentos. A CLI pede:

1. **Base URL** — default `https://keys.barroso.tec.br/v1`; pressione Enter para manter ou informe outro endpoint OpenAI Chat Completions compatível.
2. **API key** — lida sem eco no terminal.
3. **Modelo** — busca `GET /models`, filtra modelos de conversa, ordena alfabeticamente e permite escolher pelo número ou ID.
4. **Reasoning effort** — inicia desativado; você pode ativar durante a conversa se o provider/modelo aceitar.

A chave, base URL, provider e model ativos são gravados em `~/.barroso-harness/config.json` localmente (arquivo `0600` quando suportado; diretório `0700`). A chave não deve ser copiada para código, README, Git ou logs. `BARROSO_HARNESS_HOME` pode mudar somente o diretório de dados. `-init` cria a estrutura de forma não destrutiva.

```bash
bh                                  # onboarding (primeira execução) ou REPL
bh -p "liste os arquivos e diga o que você vê"  # um turno sem interação; usa config salva
bh -version
bh -init
```

Comandos dentro da sessão:

```text
/model                 lista modelos text-capable, alfabético, paginado (pg<N> navega)
/model barroso-chat    troca diretamente se o ID estiver no catálogo
/effort                mostra valor atual e níveis comuns (low, medium, high, xhigh, max)
/effort high           envia reasoning_effort=high nas próximas requisições
/effort -              limpa; reasoning_effort fica omitido (sem fallback silencioso)
/provider              troca base URL + chave; valida /models antes de salvar
/new                   limpa o transcript da sessão (conversa nova, config intacta)
/update                baixa e instala a release mais nova do canal
/help
/exit                  também /quit
```

### `/update`

O canal são as **releases do repositório privado**, publicadas pelo deployer. Por serem privadas, o `/update` exige token **no ambiente** — `GITHUB_TOKEN` (ou `TOKEN_GITHUB`), nesta ordem. A chave nunca vai para a `config.json` nem para a URL: ela trafega só no header `Authorization`.

```bash
export GITHUB_TOKEN=ghp_...      # ou TOKEN_GITHUB
bh                               # dentro do REPL:
/update
```

Antes de instalar qualquer coisa, o ciclo prova três vezes que o arquivo é o que diz ser:

1. **SHA256** conferido contra o `SHA256SUMS` publicado na mesma release — baixa e compara enquanto escreve, e apaga o staged se divergir.
2. **Tamanho** conferido contra o metadata do asset.
3. **Exec-check**: o binário staged é executado (`-version`) e a resposta tem de conter a tag esperada. É o que impede que um HTML de erro de canal seja instalado como executável.

Se a release não publicar `SHA256SUMS`, ou se ele não listar o binário desta plataforma, o update **aborta** em vez de seguir "confiando". Em qualquer falha anterior à instalação, o executável em uso fica intacto — um update que erra não deixa a máquina sem harness.

A versão escolhida é a de **número máximo** entre as releases estáveis, não a primeira da lista: a API devolve por ordem de criação, e uma republicação pode colocar uma versão mais antiga à frente. Confiar na posição ofereceria downgrade. `v0.1.0` (estável) é mais nova que `v0.1.0-bh.10`, e o sufixo compara numericamente — `bh.10` > `bh.9`.

No Windows, o executável em execução não pode ser renomeado. Quando isso acontece, o comando imprime o `Move-Item` do PowerShell para rodar depois; ele também troca o alias `bh.exe`/`barroso-harness.exe`, que são dois arquivos e precisam de dois staged.

A aceitação dos níveis de effort depende do provider/modelo. Se uma requisição for rejeitada, o erro é mostrado sem downgrade automático; use `/effort -` para limpar. Trocas de modelo/provider mantêm as mensagens textuais confirmadas; tool calls pendentes nunca ficam órfãs no transcript.

`bh -p` não pergunta nada: se faltar provider/chave/model, encerra com instrução para iniciar `bh` interativo.

## ACP — modo editor

`bh -acp` serve o **Agent Client Protocol** (JSON-RPC 2.0, newline-delimited) sobre stdio, para um editor ou cliente dirigir os turnos:

```bash
bh -acp
```

`stdout` é fio de protocolo — nada de banner ou log nele; diagnóstico vai para `stderr`. Métodos: `initialize`, `session/new`, `session/prompt`, `session/cancel` e a notificação `session/update` (incluindo `tool_call`, para o cliente renderizar as tools em tempo real).

`session/new` devolve `sessionId` **sempre preenchido** e grava a sessão no mesmo `sessions.db` do REPL — uma conversa começada num editor é retomável no terminal com `/resume`. Cada sessão recebe o mesmo wiring de subsistemas (MCP, subagentes) que o REPL monta, então o cliente vê o catálogo completo de tools, e nada de `task_spawn` para o filho (delegação não é recursiva).

Falha de um servidor MCP vira aviso em `stderr` e não derruba o protocolo; a config é capturada uma vez, então `session/new` não relê o disco.

## Experiência no terminal

A sessão interativa (`bh` sem argumentos, em terminal real) usa a roupa TUI:

```text
Barroso Harness
digite /help para os comandos · texto livre vai ao modelo

provider keys-barroso · effort high · model barroso-chat · janela 250k
> barroso-chat ·high
▶ terminal_run 120ms ok
  exit_code: 0
tokens ↑1200 ↓45 · 890ms
```

Levando em conta o que você digita: o prompt tem espaço antes do marcador de effort, e a status line é **recalculada a cada iteração** — `/effort high` e `/model` refletem na hora, sem depender do resumo do comando anterior.

A paleta ANSI-256 e os glifos (`▶` tool, `✗` erro, `✓` ok, `⚠` aviso, `◆` guardrail) só são emitidos quando `Out` e `ErrOut` são terminais reais — `x/term.IsTerminal`, sem framework de TUI. `NO_COLOR` (com valor) e `TERM=dumb` desligam a cor mesmo em terminal.

O que muda quando a saída **não** é terminal — pipe (`bh | cat`), redirecionamento (`bh > log`), entrada por arquivo (`bh < cmds`) e `-p`: cai no renderer **puro**, texto cru, um caractere de escape sequer. É por isso que as receitas do site e o log de deploy ficam limpos, e por isso que os testes veem sempre a mesma substring literal (`[tool terminal_run ok`).

## Desenvolvimento

```bash
go vet ./... && go test ./... && go build ./...
```

Releases são automáticas: push na `main` → webhook do `deploy.barroso.tec.br` → binários publicados como release (sem GitHub Actions — a conta proíbe; o contrato é o `.deploy/release.json`).
