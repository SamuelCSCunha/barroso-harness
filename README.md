# Barroso Harness

Harness de agentes em **Go**, feita do zero: um binário, sem base de terceiros, para Windows/macOS/Linux.

- **Execução YOLO permanente**: as tools rodam sem prompts de aprovação, allowlist ou allow/ask/deny. Limites técnicos (timeout, teto de saída, cancelamento) são operacionais — não são permissionamento.
- **Provider configurável** no padrão **OpenAI Chat Completions** (`base_url` + chave + model), streaming SSE com `include_usage`.
- **`BARROSO.md`** entra como instrução de sistema: do diretório do projeto (e seus pais) e/ou o global em `~/.barroso-harness/BARROSO.md`.
- **Contexto com guardrail**: orçamento por janela do modelo, estimador calibrável pelo `usage` real e compactação proativa/reativa monotônica. O transcript do turno nunca é editado — a poda reduz só o request.
- **Instalação e receitas**: https://harness.barroso.tec.br

## Instalar

Binários das releases (5 plataformas + `SHA256SUMS`): a página do site tem os comandos prontos. Ou construir do fonte:

```bash
git clone https://github.com/ruanbarroso/barroso-harness
cd barroso-harness && go build -o barroso-harness ./cmd/barroso-harness
```

## Configurar (60 segundos)

```bash
barroso-harness -init          # cria ~/.barroso-harness/{config.json,BARROSO.md}
export BARROSO_API_KEY=sua-chave
# edite ~/.barroso-harness/config.json: base_url e model
```

Config mínima:

```json
{
  "base_url": "https://seu-endpoint.example/v1",
  "model": "seu-modelo"
}
```

Campos opcionais: `context_window` (janela em tokens; o guardrail fica inativo sem ela), `max_tokens`, `bytes_per_token` (calibragem manual do estimador).

## Usar

```bash
barroso-harness -p "liste os arquivos e diga o que você vê"
barroso-harness -version
```

Modo interativo, sessões persistentes, ACP, MCP e skills entram nos próximos slices — o roadmap está no plano do projeto.

## Desenvolvimento

```bash
go vet ./... && go test ./...
```

Releases são automáticas: push na `main` → webhook do `deploy.barroso.tec.br` → binários publicados como release (sem GitHub Actions — a conta proíbe; o contrato é o `.deploy/release.json`).
