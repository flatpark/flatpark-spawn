/* Calls one member of the exec family the way an app would, so that the
 * matrix exercises libflatpark-spawn.so's hooks directly; and times them.
 *   exec-probe execvp|execve|execlp|execl|spawnp|spawn CMD [ARG...]
 *   exec-probe bench N spawnp|spawn CMD [ARG...]   median microseconds
 * A failed exec prints "MODE: <strerror>" and exits 111. */
#define _GNU_SOURCE
#include <errno.h>
#include <spawn.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/wait.h>
#include <time.h>
#include <unistd.h>

extern char **environ;

static int fail(const char *what, int err) {
    fprintf(stderr, "%s: %s\n", what, strerror(err));
    return 111;
}

static int status_code(int status) {
    if (WIFEXITED(status)) return WEXITSTATUS(status);
    if (WIFSIGNALED(status)) return 128 + WTERMSIG(status);
    return 125;
}

static int spawn_wait(int path_search, char **child, int *code) {
    pid_t pid;
    int rc = path_search ? posix_spawnp(&pid, child[0], NULL, NULL, child, environ)
                         : posix_spawn(&pid, child[0], NULL, NULL, child, environ);
    if (rc != 0) return rc;
    int status;
    while (waitpid(pid, &status, 0) < 0)
        if (errno != EINTR) return errno;
    *code = status_code(status);
    return 0;
}

static int cmp(const void *a, const void *b) {
    double x = *(const double *)a, y = *(const double *)b;
    return x < y ? -1 : x > y;
}

static int bench(int n, const char *mode, char **child) {
    double *t = calloc((size_t)n, sizeof(*t));
    if (!t) return 2;
    int path_search = strcmp(mode, "spawnp") == 0;
    for (int i = 0; i < n; i++) {
        struct timespec a, b;
        int code = 0;
        clock_gettime(CLOCK_MONOTONIC, &a);
        int rc = spawn_wait(path_search, child, &code);
        clock_gettime(CLOCK_MONOTONIC, &b);
        (void)rc;
        (void)code;
        t[i] = (double)(b.tv_sec - a.tv_sec) * 1e6 + (double)(b.tv_nsec - a.tv_nsec) / 1e3;
    }
    qsort(t, (size_t)n, sizeof(*t), cmp);
    printf("%.0f\n", t[n / 2]);
    free(t);
    return 0;
}

int main(int argc, char **argv) {
    if (argc >= 5 && strcmp(argv[1], "bench") == 0) return bench(atoi(argv[2]), argv[3], &argv[4]);
    if (argc < 3) {
        fprintf(stderr, "usage: exec-probe MODE CMD [ARG...]\n");
        return 2;
    }
    const char *mode = argv[1];
    char **child = &argv[2];
    if (strcmp(mode, "execvp") == 0) execvp(child[0], child);
    else if (strcmp(mode, "execve") == 0) execve(child[0], child, environ);
    else if (strcmp(mode, "execlp") == 0) execlp(child[0], child[0], child[1], (char *)NULL);
    else if (strcmp(mode, "execl") == 0) execl(child[0], child[0], child[1], (char *)NULL);
    else if (strcmp(mode, "spawnp") == 0 || strcmp(mode, "spawn") == 0) {
        int code = 0;
        int rc = spawn_wait(strcmp(mode, "spawnp") == 0, child, &code);
        return rc ? fail(mode, rc) : code;
    } else {
        fprintf(stderr, "unknown mode %s\n", mode);
        return 2;
    }
    return fail(mode, errno);
}
