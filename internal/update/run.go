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
		return fmt.Errorf("update channel unavailable: %w", err)
	}

	cmp, err := compararVersoes(versionAtual, lanc.Version)
	if err != nil {
		return fmt.Errorf("compare versions: %w", err)
	}
	switch cmp {
	case 0:
		fmt.Fprintf(out, "already on the latest version (%s).\n", versionAtual)
		return nil
	case 1:
		fmt.Fprintf(out, "installed version (%s) is newer than the published one (%s); nothing to do.\n",
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
			return fmt.Errorf("could not read the release SHA256SUMS (%v); aborting for safety", errSomas)
		}
		soma = somas[asset.Name]
		if soma == "" {
			return fmt.Errorf("SHA256SUMS of release %s does not list %s; aborting for safety",
				lanc.Version, asset.Name)
		}
	} else {
		return errors.New("release did not publish SHA256SUMS; aborting for safety")
	}

	if confirm != nil && !confirm() {
		fmt.Fprintln(out, "update canceled; current installation kept.")
		return nil
	}

	staging := filepath.Join(stagingRoot(), asset.Name)
	defer removeIfExists(staging)

	fmt.Fprintf(out, "downloading %s (%s, ~%d MiB)...\n", asset.Name, lanc.Version, asset.Bytes/(1<<20))
	if _, err := Baixar(ctx, asset, soma, staging); err != nil {
		return err
	}
	if err := verifyStaged(staging, lanc.Version); err != nil {
		return fmt.Errorf("new binary failed the exec check: %w", err)
	}

	resultado, err := installBinary(staging)
	if err != nil {
		if len(resultado.commands) > 0 {
			fmt.Fprintln(out, "not enough permission to replace the binary; run the following, then retry /update:")
			fmt.Fprintln(out, installCommands(resultado))
			return err
		}
		return err
	}
	fmt.Fprintf(out, "installed %s; exit and reopen bh to use the new version.\n", lanc.Version)
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
