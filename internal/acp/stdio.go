// Servidor ACP sobre stdio: o modo como um cliente ACP de verdade fala com a
// harness. main.go mais tarde chama Sirve (a integração é de outro agente);
// este arquivo NÃO é ligado a lugar nenhum ainda.
//
// Contrato de stdio, e a razão de cada cuidado:
//   - stdout transporta SÓ JSON-RPC newline-delimited. Qualquer outra escrita
//     (banner, log, warning de runtime) corrompe o protocolo — por isso o log
//     do agente vai para stderr, nunca para o fio.
//   - o fio é fechado ao sair. Clientes ACP detectam fim de conexão pelo EOF;
//     um descritor aberto depois do return deixa o cliente pendurado.
package acp

import (
	"bufio"
	"context"
	"os"

	"github.com/ruanbarroso/barroso-harness/internal/config"
)

// logParaStderr é o logger padrão do agente em stdio: diagnóstico em stderr,
// nunca no fio do protocolo.
func logParaStderr(linha string) {
	w := bufio.NewWriter(os.Stderr)
	_, _ = w.WriteString(linha + "\n")
	_ = w.Flush()
}

// Sirve roda o agente ACP sobre os stdin/stdout do processo até o cliente
// fechar, o ctx ser cancelado ou a escrita falhar.
//
// A config é lida UMA vez aqui, não a cada session/new: um cliente que troca o
// modelo no arquivo no meio de uma conversa não veria efeito neste processo, e
// falhar isso depois é pior que deixar claro agora.
//
// No retorno, os turnos em voo são esperados antes de fechar o stdout: uma
// resposta cortada no meio seria pior que alguns milissegundos de atraso. O
// cancelamento do ctx (Ctrl+C do host, encerramento do cliente) já desarmou os
// Send pendurados, então essa espera não trava — a não ser por um provider que
// ignore o ctx, caso em que o host fecha o stdin e o processo morre com o pai.
func Sirve(ctx context.Context, cfg config.Config) error {
	if ctx == nil {
		ctx = context.Background()
	}
	a := Novo(cfg).ComLog(logParaStderr)
	erro := a.Handle(ctx, os.Stdin, os.Stdout)
	a.Esperar() // respostas completas antes do fechamento
	_ = os.Stdout.Close()
	return erro
}
