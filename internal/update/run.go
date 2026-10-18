package update

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// Confirmar decide se a atualização segue. Existe para o REPL perguntar e para
// os testes fixarem a resposta. Nil = segue sem perguntar (modo -p, teste).
type Confirmar func() bool

// Rodar confere o canal, baixa a release mais nova, verifica e instala. É o
// único ponto de entrada que a CLI chama.
//
// A confirmação vem ANTES do download: não há razão para baixar 11 MB e só
// depois descobrir que a pessoa não queria trocar o binário.
//
// Erros possíveis, e o que cada um significa:
//   - token ausente          → canal privado sem credencial; nada foi feito
//   - canal recusou o token  → permissão; nada foi feito
//   - sem release estável    → canal vazio nesta plataforma; nada foi feito
//   - soma divergente        → download corrompido; staged apagado, binário intacto
//   - binário em uso         → Windows não renomeia executável em execução; sai
//     com o comando PowerShell para a pessoa rodar
//
// Em todos os casos de falha ANTES da instalação, o executável em uso fica
// intocado. Um update que erra nunca deixa a máquina sem harness.
func Rodar(ctx context.Context, out io.Writer, versionAtual string, confirm Confirmar) error {
	lanc, err := BuscarUltima(ctx)
	if err != nil {
		return fmt.Errorf("canal de atualização indisponível: %w", err)
	}

	cmp, err := compararVersoes(versionAtual, lanc.Version)
	if err != nil {
		return fmt.Errorf("comparar versões: %w", err)
	}
	switch cmp {
	case 0:
		fmt.Fprintf(out, "já está na última versão (%s).\n", versionAtual)
		return nil
	case 1:
		fmt.Fprintf(out, "a versão instalada (%s) é mais nova que a publicada (%s); nada a fazer.\n",
			versionAtual, lanc.Version)
		return nil
	}

	asset, err := lanc.AssetDaPlataforma()
	if err != nil {
		return err
	}

	// A soma vem do SHA256SUMS publicado junto com a release. Sem ele, a
	// verificação de integridade fica sem referência — e baixar binário sem
	// conferir seria justamente o buraco que este pacote fecha.
	soma := ""
	if assetSomas, ok := lanc.AssetSomas(); ok {
		somas, errSomas := ListarSomas(ctx, assetSomas)
		if errSomas != nil {
			return fmt.Errorf("não deu para ler o SHA256SUMS da release (%v); abortando por segurança", errSomas)
		}
		soma = somas[asset.Name]
		if soma == "" {
			return fmt.Errorf("SHA256SUMS da release %s não lista %s; abortando por segurança",
				lanc.Version, asset.Name)
		}
	} else {
		return errors.New("release não publicou SHA256SUMS; abortando por segurança")
	}

	if confirm != nil && !confirm() {
		fmt.Fprintln(out, "atualização cancelada; instalação mantida.")
		return nil
	}

	staging := filepath.Join(stagingRoot(), asset.Name)
	defer removeIfExists(staging)

	fmt.Fprintf(out, "baixando %s (%s, ~%d MiB)...\n", asset.Name, lanc.Version, asset.Bytes/(1<<20))
	if _, err := Baixar(ctx, asset, soma, staging); err != nil {
		return err
	}
	if err := verifyStaged(staging, lanc.Version); err != nil {
		return fmt.Errorf("binário novo não passou no exec-check: %w", err)
	}

	resultado, err := installBinary(staging)
	if err != nil {
		if len(resultado.commands) > 0 {
			fmt.Fprintln(out, "faltou permissão para trocar o binário; rode e depois refaça /update:")
			fmt.Fprintln(out, installCommands(resultado))
			return err
		}
		return err
	}
	fmt.Fprintf(out, "instalado %s; saia e abra o bh de novo para usar a versão nova.\n", lanc.Version)
	return nil
}

// stagingRoot é onde o binário baixado espera a instalação. Fica no cache do
// usuário — nunca ao lado do executável, onde um arquivo temporário com bit de
// execução seria confundido com instalação pela metade.
func stagingRoot() string {
	dir, err := os.UserCacheDir()
	if err != nil {
		// Sem cache conhecido, o próprio dir do executável aceita um arquivo de
		// nome inconfundível; a instalação o remove no defer.
		if exe, errExe := os.Executable(); errExe == nil {
			return filepath.Dir(exe)
		}
		return "."
	}
	return filepath.Join(dir, "barroso-harness", "updates")
}

// normalizarVersao limpa o "dev" — build local sem versão de release. Nesse
// caso qualquer versão publicada é novidade.
func normalizarVersao(v string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return "dev"
	}
	return v
}
