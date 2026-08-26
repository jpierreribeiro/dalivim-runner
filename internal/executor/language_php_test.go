package executor

import (
	"os/exec"
	"strings"
	"testing"

	"github.com/jpierreribeiro/dalivim-runner/internal/sandbox"
	"github.com/jpierreribeiro/dalivim-runner/pkg/runnerapi"
)

// Corte 162 — camada 1: uma linguagem simples por corte.
//
// A nota do corte é explícita sobre o que não basta: "aceite NÃO é só
// hello-world: tem que provar o isolamento". Por isso os testes abaixo cobrem
// tempo, memória e as portas de escape — uma linguagem que roda mas não é
// contida não é uma linguagem suportada, é uma superfície nova.
//
// PHP foi a escolhida por um motivo verificável e não estético: é a que está
// instalada nesta máquina, então o isolamento pode ser PROVADO aqui em vez de
// escrito às cegas. Uma spec de Ruby que ninguém consegue executar seria
// exatamente o tipo de "pronto" que esta sessão passou o dia desfazendo.

func requirePHP(t *testing.T) {
	t.Helper()
	for _, bin := range []string{"php8.3", "php8.2", "php"} {
		if _, err := exec.LookPath(bin); err == nil {
			return
		}
	}
	t.Skip("php não disponível; pulando o teste de execução")
}

func newPHP(t *testing.T) *interpretedRuntime {
	t.Helper()
	// Isolamento de rede DESLIGADO aqui, como em newLua/newPython: o teste não
	// pode depender de a plataforma permitir namespaces sem privilégio. As
	// garantias da netns são exercidas de forma agnóstica em netns_test.go.
	sb, err := sandbox.Configure("off", "off", "off", "")
	if err != nil {
		t.Fatalf("configure sandbox: %v", err)
	}
	return NewPHP(sb, 64*1024, 256, 64, 4_000_000)
}

func TestLanguagePHP_HelloWorld(t *testing.T) {
	requirePHP(t)
	res := run(t, newPHP(t), runnerapi.RunRequest{
		SourceCode: "<?php echo \"hello\\n\";",
		TimeoutMs:  3000,
		MemoryMB:   128,
	})
	if res.Status != runnerapi.StatusSuccess {
		t.Fatalf("status = %q, quero sucesso (stderr=%q)", res.Status, res.Stderr)
	}
	if res.Stdout != "hello\n" {
		t.Fatalf("stdout = %q, quero %q", res.Stdout, "hello\n")
	}
}

func TestLanguagePHP_Stdin(t *testing.T) {
	requirePHP(t)
	res := run(t, newPHP(t), runnerapi.RunRequest{
		SourceCode: "<?php echo strtoupper(stream_get_contents(STDIN));",
		Stdin:      "abc",
		TimeoutMs:  3000,
		MemoryMB:   128,
	})
	if res.Status != runnerapi.StatusSuccess || res.Stdout != "ABC" {
		t.Fatalf("eco do stdin falhou: status=%q stdout=%q stderr=%q",
			res.Status, res.Stdout, res.Stderr)
	}
}

// Erro de compilação (parse) é DIFERENTE de erro em execução, e o candidato
// precisa da distinção: um diz "não compila", o outro "compila e quebra".
func TestLanguagePHP_CompileError(t *testing.T) {
	requirePHP(t)
	res := run(t, newPHP(t), runnerapi.RunRequest{
		SourceCode: "<?php echo \"sem fechar;",
		TimeoutMs:  3000,
		MemoryMB:   128,
	})
	if res.Status == runnerapi.StatusSuccess {
		t.Fatalf("um arquivo que não faz parse foi aceito como sucesso")
	}
	if res.ExitCode == 0 {
		t.Fatalf("código de saída zero para um erro de parse")
	}
	// A mensagem vai para stderr, não para o stdout que o exercício compara —
	// é a razão do display_errors=stderr. Sem isso, um aviso do PHP reprovaria
	// uma solução correta.
	if strings.TrimSpace(res.Stderr) == "" {
		t.Errorf("o erro não foi para stderr: stdout=%q", res.Stdout)
	}
}

func TestLanguagePHP_RuntimeError(t *testing.T) {
	requirePHP(t)
	res := run(t, newPHP(t), runnerapi.RunRequest{
		SourceCode: "<?php throw new RuntimeException('boom');",
		TimeoutMs:  3000,
		MemoryMB:   128,
	})
	if res.Status != runnerapi.StatusRuntimeError {
		t.Fatalf("status = %q, quero runtime_error (stderr=%q)", res.Status, res.Stderr)
	}
	if !strings.Contains(res.Stderr, "boom") {
		t.Errorf("stderr não traz a mensagem: %q", res.Stderr)
	}
}

// O TETO DE TEMPO é respeitado. Sem ele, um laço infinito prende um slot do
// runner e a fila para todo mundo.
func TestLanguagePHP_TimeoutRespected(t *testing.T) {
	requirePHP(t)
	res := run(t, newPHP(t), runnerapi.RunRequest{
		SourceCode: "<?php while (true) {}",
		TimeoutMs:  500,
		MemoryMB:   128,
	})
	if res.Status != runnerapi.StatusTimeout {
		t.Fatalf("status = %q, quero timeout (stderr=%q)", res.Status, res.Stderr)
	}
}

// O TETO DE MEMÓRIA é do JAIL, não do PHP. memory_limit=-1 é deliberado: dois
// limites concorrentes dariam duas mensagens diferentes para a mesma causa, e a
// classificação de OOM deixaria de ser determinística.
func TestLanguagePHP_MemoryCapRespected(t *testing.T) {
	requirePHP(t)
	res := run(t, newPHP(t), runnerapi.RunRequest{
		// Cresce uma string até estourar. Determinístico e sem depender de
		// alocação de estruturas internas do interpretador.
		SourceCode: "<?php $s = 'x'; while (true) { $s .= $s; }",
		TimeoutMs:  10_000,
		MemoryMB:   64,
	})
	if res.Status == runnerapi.StatusSuccess {
		t.Fatalf("uma bomba de memória terminou com sucesso")
	}
	// O que importa é TERMINAR contido e não travar a máquina; o rótulo exato
	// depende de haver cgroup, e prender o teste a um deles o tornaria frágil
	// justamente na diferença entre ambientes que ele deveria tolerar.
	if res.Status != runnerapi.StatusMemoryExceeded &&
		res.Status != runnerapi.StatusRuntimeError &&
		res.Status != runnerapi.StatusTimeout {
		t.Fatalf("status inesperado para bomba de memória: %q (stderr=%q)",
			res.Status, res.Stderr)
	}
}

// As portas de escape que não dependem de rede estão fechadas. É a SEGUNDA
// camada: o jail já bloqueia processo e rede, e esta é a que sobra se a
// primeira estiver mal configurada numa máquina de desenvolvimento.
func TestLanguagePHP_EscapeHatchesAreDisabled(t *testing.T) {
	requirePHP(t)
	res := run(t, newPHP(t), runnerapi.RunRequest{
		SourceCode: "<?php echo shell_exec('echo escapou');",
		TimeoutMs:  3000,
		MemoryMB:   128,
	})
	if strings.Contains(res.Stdout, "escapou") {
		t.Fatalf("shell_exec executou: stdout=%q", res.Stdout)
	}
}

// O php.ini da imagem não influencia o resultado. Sem `-n`, uma imagem
// reconstruída com outro pacote mudaria a saída de uma submissão sem que
// ninguém tivesse tocado no código.
func TestLanguagePHP_IgnoresTheImagePhpIni(t *testing.T) {
	requirePHP(t)
	res := run(t, newPHP(t), runnerapi.RunRequest{
		SourceCode: "<?php echo php_ini_loaded_file() === false ? 'sem-ini' : 'com-ini';",
		TimeoutMs:  3000,
		MemoryMB:   128,
	})
	if res.Status != runnerapi.StatusSuccess {
		t.Fatalf("status = %q (stderr=%q)", res.Status, res.Stderr)
	}
	if res.Stdout != "sem-ini" {
		t.Fatalf("o php.ini da imagem foi lido: %q", res.Stdout)
	}
}

// O fuso é fixo, como o locale: sem isso o mesmo script daria saídas
// diferentes em máquinas diferentes, e "determinístico" deixaria de valer.
func TestLanguagePHP_TimezoneIsPinned(t *testing.T) {
	requirePHP(t)
	res := run(t, newPHP(t), runnerapi.RunRequest{
		SourceCode: "<?php echo date_default_timezone_get();",
		TimeoutMs:  3000,
		MemoryMB:   128,
	})
	if res.Stdout != "UTC" {
		t.Fatalf("fuso = %q, quero UTC (stderr=%q)", res.Stdout, res.Stderr)
	}
}

// A política de arquivos recusa o que o runner não conseguiria executar de
// qualquer jeito: instalar dependência é proibido por desenho (a netns é
// vazia), e aceitar composer.json daria a impressão de que não é.
func TestFilePolicyPHPRefusesDependencyManifests(t *testing.T) {
	policy, ok := filePolicies["php"]
	if !ok {
		t.Fatal("php não tem política de arquivos")
	}
	for _, proibido := range []string{"composer.json", "composer.lock"} {
		if _, barrado := policy.ForbiddenNames[proibido]; !barrado {
			t.Errorf("%s não é recusado", proibido)
		}
	}
	if _, barrado := policy.ForbiddenComponents["vendor"]; !barrado {
		t.Error("o diretório vendor não é recusado")
	}
	if policy.DefaultEntry != "main.php" {
		t.Errorf("entrada padrão = %q, quero main.php", policy.DefaultEntry)
	}
}
