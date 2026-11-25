/* A tool only the sandbox has (like an app's /app/bin helper). */
#include <stdio.h>
#include <string.h>
int main(int argc, char **argv) {
    if (argc < 2 || strcmp(argv[1], "-q") != 0) puts("own-tool-ok");
    return 0;
}
