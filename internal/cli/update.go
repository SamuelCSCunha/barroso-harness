// /update: baixa a release mais nova do canal e troca o binário instalado.
//
// O comando é deliberadamente FORA do REPL: a instalação troca o executável que
// está rodando, e no Windows isso exige que o processo saia antes. Rodar a
// troca dentro do loop interativo herdaria um lock que não se resolve — o
// handler só orquestra, e internal/update decide o que é possível.
//
// A pergunta vem ANTES do download (via Confirmar), para ninguém esperar 11 MB
// para descobrir que não queria trocar.
package cli

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/ruanbarroso/barroso-harness/internal/update"
)

// Versao é a versão deste binário, injetada pelo main (-X main.Versao). Fica
// aqui como variável de pacote porque o REPL não recebe a versão por
// argumento, e um /update que não sabe a própria versão compararia contra
// string vazia.
var Versao = "dev"

// timeoutUpdate é o prazo do canal: consulta + download de ~11 MB + exec-check.
// Conexão ruim precisa de folga; 10 min é o teto que ainda não parece travado
// para quem está olhando o terminal.
const timeoutUpdate = 10 * time.Minute

// cmdUpdate roda o ciclo completo. Falha do canal NUNCA derruba o REPL: o erro
// volta como mensagem e a sessão continua — atualização é conveniência.
func cmdUpdate(io IO) {
	versao := strings.TrimSpace(Versao)
	ctx, cancel := context.WithTimeout(context.Background(), timeoutUpdate)
	defer cancel()

	err := update.Rodar(ctx, io.Out, versao, func() bool {
		return confirmar(io, "atualizar o barroso-harness agora?")
	})
	if err != nil {
		erroFmt(io.ErrOut, "atualização não concluída", err)
		return
	}
}

// confirmar faz a pergunta de sim/não e devolve true só para resposta
// afirmativa explícita. Enter vazio é NÃO: quem não respondeu não consentiu, e
// trocar binário por omissão seria o default mais perigoso possível.
//
// EOF (Ctrl+D) no meio da pergunta também é não — a linha volta limpa, como no
// resto do REPL.
func confirmar(io IO, pergunta string) bool {
	fmt.Fprintf(io.Out, "%s %s ", verdof("❯"), pergunta)
	linha, err := io.Reader().ReadString('\n')
	if err != nil {
		fmt.Fprintln(io.Out)
		return false
	}
	resposta := strings.ToLower(strings.TrimSpace(linha))
	return resposta == "s" || resposta == "sim" || resposta == "y" || resposta == "yes"
}
