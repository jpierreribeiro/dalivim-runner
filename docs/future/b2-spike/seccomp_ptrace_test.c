/* SPIKE B.2 — a pergunta que decide se um tracer pode viver dentro do jail.
 *
 * O denylist do runner mata `ptrace` justamente como "tracer attach". Se a gente
 * abrir ptrace para o tutor, o CÓDIGO DO ALUNO ganha ptrace também (é a mesma
 * política). Este programa mede se isso é explorável:
 *
 *   1. instala um filtro seccomp no estilo do runner: DEFAULT ALLOW, KILL numa
 *      syscall escolhida (aqui `getpid`, no lugar de mount/io_uring_setup);
 *   2. o filho faz PTRACE_TRACEME e chama uma syscall PERMITIDA (getppid);
 *   3. o pai, na parada de entrada de syscall, REESCREVE orig_rax para a syscall
 *      PROIBIDA e deixa seguir;
 *   4. se a syscall proibida executar, o filtro foi contornado: seccomp roda
 *      ANTES da parada do ptrace e a syscall reescrita não é refiltrada.
 *
 * Saída: BYPASS (executou a proibida) ou CONTIDO (morreu de SIGSYS).
 * Compilar: gcc -O0 -o seccomp_ptrace_test seccomp_ptrace_test.c
 */
#define _GNU_SOURCE
#include <errno.h>
#include <linux/audit.h>
#include <linux/filter.h>
#include <linux/seccomp.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/prctl.h>
#include <sys/ptrace.h>
#include <sys/syscall.h>
#include <sys/user.h>
#include <sys/wait.h>
#include <unistd.h>
#include <signal.h>
#include <stddef.h>

#define PROIBIDA __NR_mount /* a mesma syscall que o denylist do runner mata */
#define PERMITIDA __NR_getppid /* syscall inocente que o filtro deixa passar */

static int instalar_filtro(void) {
	struct sock_filter filtro[] = {
		/* carrega arch */
		BPF_STMT(BPF_LD | BPF_W | BPF_ABS, offsetof(struct seccomp_data, arch)),
		BPF_JUMP(BPF_JMP | BPF_JEQ | BPF_K, AUDIT_ARCH_X86_64, 1, 0),
		BPF_STMT(BPF_RET | BPF_K, SECCOMP_RET_KILL_PROCESS),
		/* carrega nr */
		BPF_STMT(BPF_LD | BPF_W | BPF_ABS, offsetof(struct seccomp_data, nr)),
		BPF_JUMP(BPF_JMP | BPF_JEQ | BPF_K, PROIBIDA, 0, 1),
		BPF_STMT(BPF_RET | BPF_K, SECCOMP_RET_KILL_PROCESS),
		BPF_STMT(BPF_RET | BPF_K, SECCOMP_RET_ALLOW),
	};
	struct sock_fprog prog = {
		.len = (unsigned short)(sizeof(filtro) / sizeof(filtro[0])),
		.filter = filtro,
	};
	if (prctl(PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0) != 0) return -1;
	return prctl(PR_SET_SECCOMP, SECCOMP_MODE_FILTER, &prog);
}

int main(void) {
	pid_t filho = fork();
	if (filho < 0) { perror("fork"); return 2; }

	if (filho == 0) {
		if (instalar_filtro() != 0) { perror("seccomp"); _exit(3); }
		if (ptrace(PTRACE_TRACEME, 0, 0, 0) != 0) { perror("TRACEME"); _exit(4); }
		raise(SIGSTOP);
		/* syscall permitida pelo filtro; o pai vai reescrevê-la */
		syscall(PERMITIDA);
		_exit(0);
	}

	int status;
	waitpid(filho, &status, 0); /* SIGSTOP inicial */
	ptrace(PTRACE_SETOPTIONS, filho, 0, PTRACE_O_EXITKILL);

	int reescrita_feita = 0;
	for (;;) {
		if (ptrace(PTRACE_SYSCALL, filho, 0, 0) != 0) { printf("PTRACE_SYSCALL falhou: %s\n", strerror(errno)); break; }
		if (waitpid(filho, &status, 0) < 0) { printf("waitpid falhou: %s\n", strerror(errno)); break; }
		if (WIFEXITED(status)) {
			printf("filho saiu normalmente (code=%d)\n", WEXITSTATUS(status));
			printf("RESULTADO: %s\n", reescrita_feita ? "BYPASS — a syscall proibida executou apos a reescrita"
			                                          : "inconclusivo (nao houve reescrita)");
			return reescrita_feita ? 1 : 0;
		}
		if (WIFSIGNALED(status)) {
			printf("filho morto por sinal %d (%s)\n", WTERMSIG(status), strsignal(WTERMSIG(status)));
			printf("RESULTADO: CONTIDO — o filtro pegou a syscall reescrita\n");
			return 0;
		}
		if (!WIFSTOPPED(status)) continue;

		struct user_regs_struct regs;
		if (ptrace(PTRACE_GETREGS, filho, 0, &regs) != 0) { printf("GETREGS falhou: %s\n", strerror(errno)); continue; }
		printf("  stop: orig_rax=%lld\n", (long long)regs.orig_rax);
		if (!reescrita_feita && (long)regs.orig_rax == PERMITIDA) {
			printf("parada em syscall permitida nr=%lld -> reescrevendo para a PROIBIDA nr=%d\n",
			       (long long)regs.orig_rax, PROIBIDA);
			regs.orig_rax = PROIBIDA;
			if (ptrace(PTRACE_SETREGS, filho, 0, &regs) != 0) perror("SETREGS");
			reescrita_feita = 1;
		}
	}
	printf("RESULTADO: inconclusivo\n");
	return 0;
}
