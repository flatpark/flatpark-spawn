/* launch MODE PROG [ARGS...]
 *
 * Starts PROG through one member of the exec or posix_spawn family, with
 * the environment unchanged, as an app would. execle/execvpe/spawn* pass an
 * explicit copy of environ (the hook must read the envp it is given).
 * fork: fork while a second thread keeps malloc busy, exec in the child
 * (Chromium, node-pty): the hook must not take the allocator's locks.
 * execl* take at most two arguments. Exits with PROG's status, or 111 when
 * it could not start it. */
#define _GNU_SOURCE
#include <errno.h>
#include <pthread.h>
#include <spawn.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/wait.h>
#include <unistd.h>

extern char **environ;

static void *spin(void *arg) {
    (void)arg;
    for (;;) {
        free(malloc(64));
        usleep(100);
    }
    return NULL;
}

static int status_code(int st) { return WIFEXITED(st) ? WEXITSTATUS(st) : 128 + WTERMSIG(st); }

int main(int argc, char **argv) {
    if (argc < 3) return 2;
    const char *mode = argv[1];
    char **child = argv + 2;
    size_t n = 0;
    while (environ[n]) n++;
    char **env = calloc(n + 1, sizeof(*env));
    memcpy(env, environ, n * sizeof(*env));
    char *a1 = child[1], *a2 = a1 ? child[2] : NULL;
    if (!strncmp(mode, "execl", 5) && a2 && child[3]) return 2;

    if (!strcmp(mode, "execve")) execve(child[0], child, env);
    else if (!strcmp(mode, "execv")) execv(child[0], child);
    else if (!strcmp(mode, "execvp")) execvp(child[0], child);
    else if (!strcmp(mode, "execvpe")) execvpe(child[0], child, env);
    else if (!strcmp(mode, "execl")) execl(child[0], child[0], a1, a2, (char *)NULL);
    else if (!strcmp(mode, "execlp")) execlp(child[0], child[0], a1, a2, (char *)NULL);
    else if (!strcmp(mode, "execle")) execle(child[0], child[0], a1, a2, (char *)NULL, env);
    else if (!strcmp(mode, "spawn") || !strcmp(mode, "spawnp")) {
        pid_t pid;
        int rc = mode[5] ? posix_spawnp(&pid, child[0], NULL, NULL, child, env)
                         : posix_spawn(&pid, child[0], NULL, NULL, child, env);
        if (rc != 0) { fprintf(stderr, "%s: %s\n", mode, strerror(rc)); return 111; }
        int st;
        waitpid(pid, &st, 0);
        return status_code(st);
    } else if (!strcmp(mode, "fork")) {
        pthread_t t;
        pthread_create(&t, NULL, spin, NULL);
        usleep(1000);
        fflush(NULL);
        pid_t pid = fork();
        if (pid == 0) {
            execve(child[0], child, env);
            _exit(111);
        }
        int st;
        waitpid(pid, &st, 0);
        return status_code(st);
    } else return 2;
    fprintf(stderr, "%s: %s\n", mode, strerror(errno));
    return 111;
}
