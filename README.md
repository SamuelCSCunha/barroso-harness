# Barroso Harness

Harness de agentes em **Go**, feita do zero: um binário, sem base de terceiros, para Windows/macOS/Linux.

- **Execução YOLO permanente**: as tools rodam sem prompts de aprovação, allowlist ou allow/ask/deny. Limites técnicos (timeout, teto de saída, cancelamento) são operacionais — não são permissionamento.
- **Provider configurável** no padrão **OpenAI Chat Completions** (`base_url` + chave + model), streaming SSE com `include_usage`.
- **Sessão interativa** com transcript em memória e comandos `/model`, `/effort`, `/provider`, `/new`, `/help`, `/exit`. No terminal, a saída usa paleta ANSI-256 e glifos (`▶` tool, `✗` erro, `✓` ok, `◆` guardrail) com status line viva e rodapé de tokens por turno; em pipe/`-p` sai texto cru, sem escapes.
- **`BARROSO.md`** entra como instrução de sistema: do diretório do projeto (e seus pais) e/ou o global em `~/.barroso-harness/BARROSO.md`.
- **Contexto com guardrail**: orçamento por janela do modelo, estimador calibrável pelo `usage` real e compactação proativa/reativa monotônica. O transcript de cada turno só é confirmado em rodada completa.
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
/help
/exit                  também /quit
```

A aceitação dos níveis de effort depende do provider/modelo. Se uma requisição for rejeitada, o erro é mostrado sem downgrade automático; use `/effort -` para limpar. Trocas de modelo/provider mantêm as mensagens textuais confirmadas; tool calls pendentes nunca ficam órfãs no transcript.

`bh -p` não pergunta nada: se faltar provider/chave/model, encerra com instrução para iniciar `bh` interativo.

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
