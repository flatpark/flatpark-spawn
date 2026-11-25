# flatpark-spawn 设计与实测

> 原型 (2026-11-23) 叫 hostexec, 2026-11-24 改名 flatpark-spawn, 交付形式改为多应用共用的 Flatpak 扩展 `org.flatpark.Spawn`。本文是第一版的设计与实测, 原型的实测记录压缩在文末「原型记录」。

## 结论摘要

- **共享扩展可行** (见「共享扩展机制的验证」): 一个 `runtime/org.flatpark.Spawn/<arch>/1` 挂进了 freedesktop 26.08 和 GNOME 51 两个应用的 `/app/lib/flatpark-spawn`, 装应用时从同一 remote 自动带上, payload 走 extra-data。有三条硬约束: 消费方不能写 `autodelete: true`; 消费方构建时要 `mkdir -p /app/lib/flatpark-spawn`; apply_extra 只能用一个 runtime (取 fd 26.08)。
- **透明调用宿主工具链**: 动态链接的调用方 (C、Python、Node/libuv、bash/sh、env、xargs、Electron/zypak) 执行 bare name 时, 宿主有就到宿主执行, 用宿主登录 shell 的 PATH (mise 的 node/go/gh 都能找到); 只有宿主没有、或者参数里有沙箱私有路径时, 才留在沙箱。e2e 矩阵 60 项在两个 runtime 上全过。
- **登录 PATH 按工作目录区分**: mise/asdf/direnv 按项目改 PATH, 所以登录 shell 在命令的 cwd 里运行, 宿主和沙箱两侧的缓存都按 cwd 分开。新目录第一次调用多付一次登录 shell (本机约 75 ms)。
- **性能** (本机实测中值): 转发一次 `true` 4.7–5.2 ms (目标 6 ms, 见「性能」); 宿主没有的 bare name 从第二次起约 0.46 ms (就是本地执行本身); 不转发的本地 exec 只多 40–80 µs。
- **授权**: 应用没有 `--talk-name=org.freedesktop.Flatpak` 时, enable.sh 的 probe 失败, 什么都不设置, 行为和没装扩展完全一样 (e2e 验证)。
- **已知不透明**: 静态链接的调用方 (Go、musl Rust/Codex)、沙箱里自己查 PATH 的非 bash shell、指向 `/app` 的回调与脚本、`fexecve`/`execveat`。

## 架构

| 组件 | 形态 | 职责 |
|---|---|---|
| `libflatpark-spawn.so` | C, `LD_PRELOAD` | 拦截 exec 家族和 posix_spawn 家族, 按判定表决定去哪执行; 导出 bash 的 `command_not_found_handle`; socket 被 seccomp 拒绝时到宿主代建 |
| `flatpark-spawn` | 静态 Go 单文件, 一个文件三种身份 | (1) 用户命令和 host-spawn 兼容入口; (2) 沙箱侧转发 (内部模式 `--x-*`); (3) 经 HostCommand 把自身送到宿主执行时的宿主侧 agent |
| `enable.sh` | POSIX sh 片段 | 消费方 launcher source 的一行; probe 通过后设置 `LD_PRELOAD`、`ZYPAK_LD_PRELOAD`、`TMPDIR`、缓存目录, 并在被 bash source 时 `export -f` 兜底函数 |
| 扩展 ref | `org.flatpark.Spawn//1` | 只有 `bin/apply_extra`、metainfo 和指向 `extra/` 的符号链接 (`enable.sh`、`bin/flatpark-spawn`、`bin/host-spawn`、`lib/libflatpark-spawn.so`); 编译产物全部是 extra-data |

一次 bare name 转发的路径 (缓存命中):

```
应用进程 execvp("git")                     沙箱                         宿主
  └ libflatpark-spawn.so: 判定 → 宿主
     └ execve(/app/lib/flatpark-spawn/bin/flatpark-spawn --x-exec git git args…)
        └ 查本 cwd 的缓存 d/<cwd>/host/git (有完整路径, 30 s 内) ─── HostCommand(/usr/bin/git args…, PATH=该目录的登录 PATH) ──→ git
        └ 等 HostCommandExited, 转发信号, 以同样的退出码/信号退出   ←── 退出状态 ───
```

缓存没命中 (第一次、超过 30 s、或者直接启动失败) 时走 agent:

```
flatpark-spawn --x-exec ─ HostCommand(/proc/self/fd/4 --x-agent exec CWD git git args…, fd 3=控制 socket, fd 4=自身 ELF) ─→ agent
                         控制 socket 第一包: 调用方的环境 (见「环境变量」)                                              ├ 在该 cwd 缓存的登录 PATH 里找 git, 找不到就刷新 (在 cwd 里跑登录 shell) 再找
                         ←─ 回复 found/none/nocwd + generation + 完整路径 + 登录 PATH + 会话环境指纹 ───────────────────┤
                         记入缓存                                                                                     └ found: execve(git) (同一个 pid)
```

调用方看到的 PID 就是 flatpark-spawn 进程: 它一直活到宿主进程退出, 然后以同样的退出码退出, 或者被同一个信号杀死, 所以在调用方眼里和本地子进程没有区别。

## 共享扩展机制的验证 (2026-11-24)

本机 Flatpak 1.18.4。最小样例: 扩展 `runtime/org.flatpark.Spawn/x86_64/1` (payload 是 extra-data tarball, 本地 `python3 -m http.server` 提供, `apply_extra` 解包到 `extra/`), 两个消费方 `org.flatpark.SpawnSpikeFd` (org.freedesktop.Platform 26.08) 和 `org.flatpark.SpawnSpikeGnome` (org.gnome.Platform 51), manifest 都写:

```yaml
add-extensions:
  org.flatpark.Spawn:
    directory: lib/flatpark-spawn
    version: '1'
```

测完全部卸载, 临时 remote 和目录已删。

**结论: 可行。** 一个扩展 ref 挂进了两个不同 runtime 的应用。

| 问题 | 实测结果 |
|---|---|
| 能否挂到 fd 26.08 和 GNOME 51 两个应用的 `/app/lib/flatpark-spawn` | ✅ 两边都能运行 `extra/` 里的 payload。挂载只按扩展点名字 + `version` 匹配, 与扩展自己的 `[ExtensionOf]` 无关 |
| `[ExtensionOf]` 写什么, 影不影响挂载 | 不影响挂载, 三种写法都试过: 没有 `[ExtensionOf]`、`ref=runtime/org.freedesktop.Platform/…/26.08`、`ref=app/<另一个消费方>`, GNOME 应用都能挂上。即使 `ref` 指向的应用已卸载, 其他消费方照样能用 |
| 扩展 manifest 用 `runtime: org.freedesktop.Platform` + `build-extension: true` | ❌ flatpak-builder 报 `No extension point matching org.flatpark.Spawn in runtime/org.freedesktop.Platform` |
| 改用 `build-runtime: true` | ❌ 能构建, 但会把整个 Sdk 当成 `/usr` 一起提交 (实测导出 1.7 GB), 不可用 |
| `build-extension: true` + `runtime:` 指向一个声明了 `org.flatpark.Spawn` 扩展点的应用 | ✅ 推荐写法, 和主仓现有扩展一样用替身父应用 (stand-in)。产出的 metadata: `[Runtime] name=org.flatpark.Spawn`; `[ExtensionOf] ref=app/<替身>/…`, `runtime=org.freedesktop.Platform/x86_64/26.08` (取替身的 runtime) |
| 手工 `flatpak build-export` (自己写 metadata) | ✅ 也行; 但 e2e 用的是替身 + 真实 manifest (`packaging/`), 这样 manifest 本身也被测到 |
| 装消费方时扩展会不会从同一 remote 自动装上 | ✅ 只要扩展点没写 `no-autodownload` (默认就会自动下载), 装消费方时会顺带安装 `runtime/org.flatpark.Spawn/x86_64/1`。所以消费方**不写** `no-autodownload` |
| `autodelete` | ⚠️ **消费方必须写 `autodelete: false` (或者干脆不写)**。实测 A、B 两个应用都装着时, 卸载 A (A 写了 `autodelete: true`), 扩展会被一起删掉, B 当场挂载不到扩展。不写时扩展留着, 等最后一个消费方也卸载后, 由 `flatpak uninstall --unused` 清掉 (实测: 还有消费方在用时报 "Nothing unused"; 消费方全卸后清掉了, 即使 Platform 26.08 仍装着) |
| extra-data + `apply_extra` | ✅ 安装时下载, 校验 sha256, `apply_extra` 在 `extra/` 里解包 |

### apply_extra 需要的 runtime

读 flatpak 1.18.4 源码 (`flatpak-dir.c` 的 `apply_extra_data`, `flatpak-transaction.c` 的 `op_get_runtime_ref`) 并实测:

- `apply_extra` 在 bwrap 里运行, `/usr` 来自 metadata `[Runtime] runtime=`, 没有这一项时用 `[ExtensionOf] runtime=`。该 runtime 必须已经安装, 否则安装失败。
- 安装事务**只会**把 `[ExtensionOf] runtime=` 当依赖自动安装 (前提是有 `[Extra Data]` 且没有 `NoRuntime`)。实测把它写成未安装的 `org.freedesktop.Platform/x86_64/25.08` 后, 安装提示 "Required runtime … found in remote flathub. Do you want to install it?", 拒绝就失败。
- 后果: 扩展只能有一个 apply_extra runtime。用户只装了 GNOME 51 应用时, 装第一个消费方会额外拉一份 `org.freedesktop.Platform//26.08` (约几百 MB)。FlatPark 大部分包用 fd 26.08, 多数用户本来就有。
- 可以避开这份依赖的写法: `[Extra Data] NoRuntime=true`。代价是 `apply_extra` 没有 `/usr`, 只能是静态 ELF 并放进扩展 ref, 违反「ref 里不放编译产物」。另一种是不要 `apply_extra`、把 payload 拆成几个 extra-data 文件直接落进 `extra/`, 但 flatpak 写这些文件时没有执行位 (`glnx_file_replace_contents_at` 默认 0644), 静态 Go 二进制就跑不起来。两条都不取。
- **采用**: `[ExtensionOf] runtime=org.freedesktop.Platform/<arch>/26.08`, `apply_extra` 用 runtime 自带的 `tar`/`sh`。runtime 升大版本时, 扩展在同一分支 `1` 上重建, 只改这一项, 消费方不用改。

### 对消费方写法的约束 (由上面的验证得出)

```yaml
add-extensions:
  org.flatpark.Spawn:
    directory: lib/flatpark-spawn
    version: '1'          # ABI 大版本, 等于扩展分支名
    # 不写 no-autodownload: 要它随应用自动安装
    # 不写 autodelete (或写 false): 写 true 会在卸载本应用时把其他应用正在用的扩展一起删掉
```

**消费方构建时必须 `mkdir -p /app/lib/flatpark-spawn`**: e2e 实测不建时 `flatpak run` 直接失败 (`bwrap: Can't create file /app/lib/flatpark-spawn: Read-only file system`), 应用根本起不来, 不论扩展装没装。flatpak-builder 不会按 `add-extensions` 自动建目录。构建扩展用的替身父应用同样要建 (否则 flatpak-builder 报 `Can't mkdir parents for /app/lib/flatpark-spawn`)。


## 判定表

实现在 `preload/route.c` (纯函数, 不含 hook, Go 单元测试经 cgo 直接调用它, 见 `internal/routetest`)。只在 enable.sh 设置了 `FLATPARK_SPAWN_ENABLE=1` 时生效; 变量从这次 exec 传的 envp 里读 (bash 的 `VAR=x cmd` 只放在 envp 里)。

| 调用 | 条件 | 去处 | 说明 |
|---|---|---|---|
| bare name (`execvp`/`execvpe`/`execlp`/`posix_spawnp`) | 在 `FLATPARK_SPAWN_LOCAL` 里 | 沙箱 | 冒号分隔, manifest 的 launcher 写死, 用来处理确实要用沙箱版本的同名命令 |
| bare name | 沙箱有, 且参数或 cwd 里有沙箱私有路径 | 沙箱 | 见下节 |
| bare name | 本 cwd 的缓存 `host/<name>` 存在 | 宿主 | 不再问宿主 |
| bare name | 本 cwd 的缓存 `none/<name>` 未过期 | 沙箱 | 负缓存, 见「查找缓存」; 沙箱也没有时就是原生 ENOENT |
| bare name | 都没命中 | 同步问一次宿主 (`--x-lookup`), 有 → 宿主, 无 → 沙箱 | 结果写入缓存 |
| `execve(path)` 且 argv[0] 是 bare name、等于 path 的 basename、path 的目录在 PATH 里 | — | 按 bare name 判定 | Python `subprocess` 和 bash 自己逐个 PATH 目录 `execve`, argv[0] 仍是 bare name (glibc 的 execvp 走内部 `__execve`, 不经过 hook)。第一个候选就转发; 判为沙箱时原样执行这个候选, 调用方会接着试下一个 |
| `execve(path)` 绝对路径, 沙箱里存在 | — | 沙箱 | 应用明确指定的文件 (argv[0] 也是绝对路径) |
| 绝对路径, 沙箱里不存在, 在私有前缀下 | — | 沙箱 (原生 ENOENT) | 例如 `/app/extra/缺失` |
| 绝对路径, 沙箱里不存在 (ENOENT/ENOTDIR) | 其他 | 宿主 | `/usr/bin/docker`、未授权的 `~/.local/bin/x`; 宿主也没有时进程已经换成 flatpark-spawn, 只能退出 127 并打印 `No such file or directory` |
| 相对路径 (`./x`, `a/b`) | — | 沙箱 | 依赖 cwd |
| `fexecve`/`execveat` | — | 不拦截 | fd 指向沙箱文件, 宿主无意义 |

**同名默认走宿主** (维护者决定): `cat`、`sh`、`env`、`python3` 这些沙箱和宿主都有的命令, 默认在宿主执行; 例外靠私有路径规则和 `FLATPARK_SPAWN_LOCAL`。`FLATPARK_SPAWN_HOST` 已删除, 默认就是宿主, 不再需要它。

**真正找不到** 和命令自己 `exit 127` 是两回事: bare name 两边都没有时, `execvp`/`posix_spawnp` 原样返回 ENOENT (Python 抛 `FileNotFoundError`), shell 打印 `command not found` 并返回 127; 命令自己的 127 是 exec 成功后的退出状态。agent 用控制 socket 上的 `none` 回复和 `found` + 退出状态区分这两种情况 (`internal/agent` 的 `TestAgentTellsNotFoundFromExit127`, e2e 的 exit code 几项)。

## 沙箱私有路径

参数 (含 `sh -c` 的命令字符串里的词, `--opt=/x`, `VAR=/x:/y`) 或 cwd 里出现下面这些路径时, 沙箱有这个命令就留在沙箱:

| 前缀 | 为什么是私有的 |
|---|---|
| `/app` | 应用自己的文件, 宿主上没有 |
| `/usr`, `/bin`, `/sbin`, `/lib`, `/lib64`, `/lib32` | 沙箱里是 runtime, 和宿主的 `/usr` 是两棵树; 参数里出现这些路径, 几乎都是应用在沙箱里看到的文件 (fc-cache、ldconfig、编译器的头文件路径)。转发过去会悄悄换成宿主上的另一个文件 |
| `/tmp` | 沙箱的 `/tmp` 是私有的。只有 `--filesystem=/tmp` 才共享 (从 `/.flatpak-info` 的 `[Context] filesystems=` 判断)。**实测 `--filesystem=host` 不共享 `/tmp`** |
| `/var/tmp` | 沙箱私有 |
| `/run/flatpak`, `/run/host`, `/.flatpak-info` | flatpak 的沙箱布局, 宿主上没有或含义不同 |
| `/dev/fd`, `/proc/self/fd` | bash 的进程替换 `<(...)`: fd 只在沙箱进程里, HostCommand 只传 0/1/2 |

**不在名单里的**:
- `/etc`: 验收要求沙箱里 `cat /etc/os-release` 得到宿主的发行版; agent 读的是宿主配置, `/etc` 下的 passwd、hosts、resolv.conf、localtime 本来就和宿主一致。
- `~/.var/app/<id>`、`$XDG_RUNTIME_DIR/app/<id>`、文档 portal 的 `/run/user/<uid>/doc/...`: 宿主上是同一个路径。
- `$HOME` 下已授权的目录: 同一路径。

匹配规则是「词边界」: 前缀前面是开头或 ` \t\n'"=:;(`,<>|&@{` 之一, 后面是结尾、`/` 或分隔符。所以 `/application`、`/usrlocal`、URL 里的 `/app/` 都不算。已知漏掉的写法: `-I/usr/include` (前面紧挨字母)。参数里有私有路径但沙箱没有这个命令时 (`node /app/x.js`, node 只在宿主) 仍然转发, 因为留在沙箱只会是 ENOENT。

## 宿主侧 agent 与登录 PATH

- 沙箱侧把 `flatpark-spawn` 自身的 ELF 用 HostCommand 的 fd map 放在宿主进程的 fd 4, argv[0] 是 `/proc/self/fd/4`。这个手法和 socket 代建相同, 不需要宿主预装任何东西。
- **登录 PATH 按工作目录区分**。mise、asdf、direnv、nvm 的 auto-use 都按目录改 PATH: 实测同一个 `zsh -lic 'echo $PATH'` 在 `/` 和带 `mise.toml` 的项目目录里顺序不同, 项目里 pin 了 node@20 时宿主终端拿到的就是 20。所以:
  - 登录 shell 在**命令的 cwd** 里运行 (`cmd.Dir` 加上 `PWD=cwd`, mise 和 direnv 看的就是它); lookup 模式也带 cwd (`lookup CWD NAME`)。
  - 缓存是每个目录一个文件: 宿主上的 `$XDG_RUNTIME_DIR/flatpark-spawn/login-path.d/<key>`, key = cwd 的 FNV-64a (16 位十六进制), 内容是「cwd、PATH、时间戳」三行。读取时核对第一行的 cwd, 哈希碰撞就当作未命中。所有应用共享, 重启后清空, 最多 256 项, 新建时删掉最旧的 (按 mtime)。
  - 失效条件沿用原来的, 只是改成按目录各自计算: 查找失败时刷新 (2 s 去抖), 超过 60 s 重新取; generation 由该目录的登录 PATH 计算。
  - 代价: 新目录第一次调用多付一次登录 shell, 本机约 75 ms (之后约 5 ms)。IDE 在项目根目录和少数子目录里调工具链, 实际只多几次。
  - 不覆盖: 只在提示符钩子里改 PATH 的工具 (direnv 的 `direnv hook` 走 precmd, 在 `-c` 模式下不运行)。mise activate 在激活时会立即按当前目录跑一次 hook-env, 所以覆盖到了 (e2e 实测)。
- 刷新方式是跑一次 `$SHELL -l -i -c 'printf …'`, fish 单独写法。为了不让 rc 文件的输出泄漏到调用方:
  - 用 `setsid` 起新会话, 没有控制终端, rc 文件写 `/dev/tty` 也写不到用户终端;
  - stdin 接 `/dev/null`, stderr 丢弃, stdout 只用来解析标记 `__FPS_LOGIN_PATH=` 后面的内容 (标记在脚本里分两段写, 回显脚本本身不会误命中);
  - 超时 10 s 后杀整个进程组; shell 退出后最多再等 stdout 200 ms (rc 文件起的守护进程可能占着它)。
  - shell 选择顺序: 会话环境的 `SHELL`, 没有就用 passwd 里的, 再没有用 `/bin/sh`。session helper 的环境经常没有 `SHELL`。
- 查找顺序: 缓存的登录 PATH 里有就用 (同一 PATH 目录里新放的命令马上能找到)。找不到时刷新一次再找, 但距上次刷新不到 2 s 就不刷 (应用启动时连续探测好几个不存在的命令, 只付一次 70 ms)。缓存超过 60 s 时, 下一次 agent 运行先刷新再查。这一条针对 `mise use node@22` 这种把 PATH 换到另一个已存在目录的情况: 旧目录还在, 不会出现查找失败。
- 宿主还是找不到时, agent 回 `none`, 沙箱侧按 ENOENT/127 处理, 不会和命令自己的 127 混淆。
- 找到后, agent 先回 `found`, 再给控制 fd 和自身 fd 设 CLOEXEC, 然后 `execve`。没有 `#!` 的脚本 (ENOEXEC) 和 execvp 一样交给 `/bin/sh`。命令的 PATH 是登录 PATH; 会话没有 `SHELL` 时补上。
- cwd 由 agent 自己 `chdir`, HostCommand 的 cwd 固定传 `/`。目录在宿主上不存在时 agent 回 `nocwd`: 转发路径改在沙箱执行 (沙箱有这个命令时), CLI 则报错。不会悄悄换成 `$HOME`。
- 原型里每次转发都跑登录 shell 的做法 (72 ms) 已删除。
- **控制 socket 的大小上限**。一个 SOCK_SEQPACKET 报文必须装得进发送方的 socket 缓冲区: 本机 `net.core.wmem_default` = 212992, 实测 204800 字节能发、212992 报 EMSGSIZE。因此两个方向的每个报文统一限制为 `proto.MaxMessage` = 128 KiB:
  - 发送超限直接报「… is N bytes, more than the control socket's limit」。调用方环境超限时转发失败, 退回本地执行或报错。
  - 接收用满额缓冲加 `MSG_TRUNC`: 更长的报文报「control message truncated: N bytes」, 不再误报 malformed。最初的测试用 4 KiB 缓冲, 9 KB 的环境 (PATH 1.1 KB) 下回复就被截断了。
  - agent 的回复放不下时, 先去掉可选的会话指纹; 还放不下就回 `error`, 并且不执行命令 (否则沙箱侧不知道命令已经启动)。

## 查找缓存 (正缓存、负缓存、直接启动)

缓存目录在 `$XDG_RUNTIME_DIR/app/$FLATPAK_ID/flatpark-spawn/` (enable.sh 设为 `FLATPARK_SPAWN_CACHE`), 沙箱内外是同一个路径, 每个应用一份, 登出后清空。只由沙箱侧写入 (`internal/sandbox/cache.go`); C 库只读。

**除会话指纹外, 一切都按工作目录分开**: 每个 cwd 的条目在 `d/<key>/` 下, key 与宿主侧相同 (cwd 的 FNV-64a)。C 里的 `fps_cwd_key` 和 Go 里的 `agent.Key` 有测试保证结果一致。两边用的都是物理 cwd (`getcwd`, Go 侧不用 `os.Getwd`, 因为它会优先返回 `$PWD`, 经过符号链接时哈希会不同)。在 A 目录解析出的完整路径、负缓存和登录 PATH 都不会给 B 目录用。`d/` 下最多 256 个目录, 超出时删最旧的。已知小偏差: `posix_spawn` 带 `addchdir` 文件动作时, C 库按父进程的 cwd 判定, helper 按切换后的 cwd 查缓存。

| 文件 | 内容 | 谁用 | 失效 |
|---|---|---|---|
| `d/<key>/host/<name>` | 宿主上的完整路径 | C: 存在就转发; Go: mtime 在 `PosTTL` (30 s) 内且 basename 等于 name 时直接启动 | generation 变化时清空; 直接启动失败就改走 agent, 由 agent 的回复覆盖 |
| `d/<key>/none/<name>` | 空; mtime 是判定时间 | C: 未过期就在沙箱执行 | 沙箱有这个命令时 60 s (`FPS_NEG_TTL_LOCAL`), 两边都没有时 3 s (`FPS_NEG_TTL_MISSING`); generation 变化时清空; mtime 在未来的条目不算数 |
| `d/<key>/gen` | 该目录的宿主 generation | Go | 每次 agent 回复都比较; 变化时只清空这个目录的条目 |
| `d/<key>/env` | 该目录的登录 PATH 和 shell | 直接启动 | 每次 agent 回复覆盖 |
| `session` | 宿主会话环境的指纹 (变量名 → 值的 FNV-64) | 直接启动 | 每次 agent 回复覆盖 |

- **generation** 由 agent 计算: 该 cwd 的登录 PATH 本身加上其中每个目录的 mtime 做 FNV-64。在宿主上新装或删除命令都会改变它。沙箱侧发现它变了, 就清空这个 cwd 的 `host/` 和 `none/`。其他 cwd 的负缓存在它们自己下一次问到 agent 时才会更新, 在那之前靠 TTL (60 s / 3 s) 兜底。
- **负缓存的两档 TTL**:
  - 负缓存要解决的是应用反复调用 `/app/bin` 里自带的工具。这类命令沙箱有, 本来就跑得了, 宿主新装同名工具后最多 60 s 才会切过去, 可以接受。
  - 两边都没有的名字只信 3 s: 测试期间刚刚失败过的名字, 在宿主上装好后 3 s 内就能用; 从没查过的名字不受影响, 马上可用 (e2e 两项都测了)。
  - 宿主上没有的命令, 第二次起不再去问宿主; e2e 实测 0.46 ms, 与不加载 preload 时的 0.42 ms 几乎一样。
- **直接启动** (性能): 正缓存在 30 s 内时, 沙箱侧直接 HostCommand 那个完整路径, 不经过 agent, 少一次 Go 进程启动和一轮往返。代价有两点: 宿主进程的 argv[0] 是完整路径而不是 bare name (basename 不变, 多调用二进制和 mise shim 都不受影响); 宿主上同名命令的优先级变化最多晚 30 s 生效, 加上登录 PATH 的 60 s, 最多约 90 s。直接启动失败 (文件不见了、cwd 不在宿主上) 就自动改走 agent。

## 环境变量

宿主进程的环境从 session helper 的环境 (宿主的 HOME、DISPLAY、D-Bus/ssh-agent 地址、XDG_*) 开始, 再叠上调用方的环境, 让应用给子进程设的变量 (`GIT_TERMINAL_PROMPT=0`、`LC_ALL=C`、`CI=1`) 能传到宿主。叠加时去掉三类:

- 描述沙箱自身的: `PATH` (换成登录 PATH)、`LD_*`、`ZYPAK_*`、`FLATPAK_*`、`FLATPARK_SPAWN_*`、`BASH_FUNC_*`、`XDG_*`、`GIO_*`、`GST_*`、`GDK_PIXBUF_*`、`VK_*`、`__EGL_*`、`PYTHONPATH`、`PKG_CONFIG_PATH`、`GI_TYPELIB_PATH`、`SSL_CERT_*`、`FONTCONFIG_*` 等 runtime 布局变量, 以及 `container`、`SHLVL`、`PWD`。
- 宿主有自己值的: `HOME`、`USER`、`SHELL`、`DBUS_SESSION_BUS_ADDRESS`、`SSH_AUTH_SOCK`、`DISPLAY`、`XAUTHORITY`、`WAYLAND_DISPLAY`、`PULSE_SERVER`、`AT_SPI_BUS_ADDRESS`。
- 值里含宿主看不到的路径的 (`/app`、`/run/flatpak`、`/run/host`、`/.flatpak-info`, 以及未共享时的 `/tmp`、`/var/tmp`): 例如 `GIT_ASKPASS=/app/bin/askpass`、`NODE_OPTIONS=--require /app/x.js`。和参数规则不同, `/usr` 不算: `EDITOR=/usr/bin/vim` 在宿主上通常指同一个文件。
- 另外去掉 `ELECTRON_RUN_AS_NODE`: 沙箱里的 Electron 应用常设这个变量, 宿主上的 Electron 应用带着它会以纯 node 方式运行。

完整名单见 `internal/sandbox/env.go`。`TMPDIR` 被 enable.sh 设为 `$XDG_RUNTIME_DIR/app/$FLATPAK_ID`, 沙箱内外是同一路径, 会照常传过去。

**传递方式**:
- 走 agent 时, 调用方环境是控制 socket 上的第一个数据包, 不放进 HostCommand 的 env。这样 agent 看到的是纯粹的会话环境, 可以把它的指纹回传。也不放进 argv: `/proc/PID/cmdline` 对所有用户可读, 会泄露 token。
- 直接启动时只传值和会话指纹不同的变量。最终环境一样, 只是不再把会话已有的变量重发一遍: 本机的 116 个变量里有 44 个是会话已有的, 发得越少 HostCommand 越快。

**host-spawn 兼容入口**照上游的语义, 只传 `-env` 列出的变量 (默认 `TERM`)。

## sh 兜底

bash 自己查 PATH 失败时不会走到 exec, 而是调用 `command_not_found_handle`。这个函数以导出函数 `BASH_FUNC_command_not_found_handle%%` 的形式进入环境, 取代原型的 `BASH_ENV` 方案 (`HOSTEXEC_ORIGINAL_BASH_ENV` 那套已删除, 也不会和应用自己的 `BASH_ENV` 冲突):

- C 库的构造函数在每个加载了它的进程里 `setenv` (不覆盖已有的), 之后所有子进程中的 bash, 包括 posix 模式的 `sh -c` 和嵌套的 `sh -c`, 都会导入。
- 但 bash 读取初始环境用的是 `main` 的 `envp`, 早于构造函数的 `setenv`, 所以 launcher 直接 exec 的那个 bash 收不到 (e2e 的「an app's sh -c」一项第一次就因此失败)。因此 enable.sh 被 bash source 时 (freedesktop runtime 的 `/bin/sh` 就是 bash) 还会 `export -f` 一次。dash 等其他 shell 跳过这一步。
- 函数体调用 `flatpark-spawn --x-shell "${0##*/}" cmd args…`: 宿主有就执行; 没有就打印 `sh: cmd: command not found`, 返回 127。也遵守 `FLATPARK_SPAWN_LOCAL`。
- 没有授权时这些都不设置, bash 打印的是原生 (本地化) 的错误信息 (e2e 验证)。

剩下的缺口: zsh/fish 这类非 bash shell 在沙箱里自己查 PATH (runtime 默认不带); `env -i`、`bash -p` 这些会把 hook 和 `LD_PRELOAD` 一起丢掉的场景。

## CLI、PTY 与 host-spawn 兼容

- `flatpark-spawn [选项] [cmd args…]`: 显式在宿主执行。没有命令时开登录 shell (`$SHELL -l`, 由 agent 执行)。选项有 `-pty`、`-no-pty`、`-cwd`、`-env`、`-version`, 一个或两个短横线都行, 到第一个非选项为止。
- 以 basename `host-spawn*` 调用时兼容上游 v1.6.2: 选项相同; `-env` 默认 `TERM` 且只传列出的变量; 没有命令时执行 `sh -c '$SHELL'`; stdout 是终端且命令不在黑名单 (`gio`/`podman`/`kde-open*`/`xdg-open`) 时分配 PTY。其他 basename 是上游的 shim: symlink 成什么名字, 就在宿主执行那个命令, 全部参数原样传过去。内部模式 `--x-*` 只认 `flatpark-spawn` 和 agent 的 `/proc/self/fd/N`, shim 不会误入。
- PTY 代码 (`internal/pty`) 来自 host-spawn 的 `pty.go` (文件头注明来源和 MIT-0), 用 golang.org/x/sys 代替了 github.com/pkg/term:
  - 在沙箱里分配 PTY, 优先用 `TIOCGPTPEER`, 不依赖 `/dev/pts` 路径;
  - slave 作为宿主进程的 0/1/2, 本端终端设成 raw, 两个方向复制字节, SIGWINCH 时同步窗口大小;
  - 相对上游的改进: HostCommand 拿到 slave 后就关掉本端那份, 宿主进程退出后 master 会读到 EOF, 剩余输出复制完再退出, 最多等 500 ms。上游退出时直接停止复制, 可能丢掉最后的输出。
- `flatpark-spawn` 只在 stdin 和 stdout **都**是终端时分配 PTY, 管道一律不分配 (PTY 会带来 `\r`); 从终端的后台作业里调用时也不分配 (见「转发路径上的终端」)。preload 的转发走内部模式, 永远不分配 PTY。
- 原型阶段看到的「前导 NUL」查清了, 是测试工具的伪影: `script … </dev/null` 在 stdin 读到 EOF 时会往终端里写它的 EOF 字符。宿主上读到的是 `\x04`, 沙箱里读到的是 `\x00`, 宿主 PTY 打开了 ECHOCTL, 把它回显成 `^@`。真实终端里没有这个现象; e2e 改为给 `script` 一个保持打开的 stdin。
- 信号: 转发的只有 HUP、INT、QUIT、TERM、USR1、USR2、ALRM、WINCH、TSTP、TTIN、TTOU、CONT, 发给宿主进程组 (失败时退回只发给该进程)。
  - 不注册全部信号: 那样每次转发要多花约 1 ms。
  - TSTP/TTIN/TTOU 转发后本进程也自停, 调用方 shell 的作业控制因此保持一致。
  - SIGKILL 杀掉本进程时, HostCommand 的 WATCH_BUS 标志让宿主进程也随之结束。
  - 宿主进程被信号杀死时, 本进程以同一个信号自杀 (e2e: Python 看到 `-15`, 宿主上的 `sh` 和它的 `sleep` 子进程都消失了)。

## 转发路径上的终端

**问题** (在真实 VS Code 里发现): VS Code 终端里的沙箱 bash 执行 `zsh` → `command_not_found_handle` → 宿主 zsh, fzf-tab 报 `/dev/tty: 没有那个设备或地址`。最初的转发路径把调用方的终端原样交给 HostCommand, 而这个 pts 已经是沙箱 bash 会话的控制终端, session helper 的 TIOCSCTTY 失败, 宿主进程没有控制终端 (`tty_nr=0`), 打开 `/dev/tty` 得到 ENXIO。受影响的是一切从终端经转发启动、需要 `/dev/tty` 的程序: sudo/ssh 的密码提示、git 的编辑器、fzf、less、vim、htop、zsh 的 zle。

**做法** (`internal/sandbox/tty.go`, preload 的 `--x-exec`、bash 兜底的 `--x-shell`、直接启动三条路径共用 `hostRun`):

- fd 0/1/2 里有终端、并且我们在它的前台进程组时, 新分配一个 PTY (复用 `internal/pty`)。是终端的那几个 fd 换成 slave, 其他 fd 原样传: `ls | fzf`、`prog | less`、`fzf < list` 的管道语义不变。
- slave 必须成为宿主进程的控制终端:
  - stdin 是 slave 时, session helper 自己会做;
  - stdin 不是终端时 (`printf x | prog`), session helper 不做, 所以把 slave 额外作为宿主 fd 5 (`agent.CttyFD`) 传过去, agent 在 exec 之前 `setsid` + `TIOCSCTTY`。直接启动做不到这一步, 这种情况下跳过直接启动, 改走 agent。
- 沙箱侧中转: 输入从调用方终端读 (stdin 不是终端时读 `/dev/tty`), 输出写到第一个是终端的 fd。原终端设 raw, 窗口大小在启动前就同步, 之后 SIGWINCH 时再同步。退出、回退到本地执行、被 SIGTSTP 停下时恢复终端设置, SIGCONT 后重新设 raw。退出时最多等 500 ms 把剩余输出拷完 (沿用 CLI 的修正), 然后关 master、恢复终端、退出。
- **改终端的 ioctl 一律在屏蔽 SIGTTOU 的锁定线程上做** (`setTermios`/`setWinsize`): POSIX 规定 SIGTTOU 被阻塞或忽略时, 后台进程的 tcsetattr 照常生效, 不发信号, 所以不会出现「被 SIGTTOU 打断 → Go 捕获 → 系统调用重启」的死循环。恢复终端 (退出、Suspend) **不再先做前台判断**: 我们设过 raw, 就必须恢复, 哪怕此刻已经被移到后台。
- 宿主进程结束后, 致命信号 (HUP、INT、QUIT、TERM、USR1/2、ALRM) 改由一个没人读的 `signal.Notify` 通道接收, 直到恢复终端、按宿主的结果退出: 否则 500 ms drain 期间到来的 SIGTERM 会让调用方的终端停在 raw。`ExitWith` 再用 `signal.Reset` 恢复要重放的那个信号。不用 `signal.Ignore`: 实测之后 Reset + 重放不再能杀死本进程, 调用方看到的是 exit 143 而不是被 SIGTERM 杀死。
- raw 和拷贝要等宿主确认找到命令 (`found`) 之后才开始。宿主没有这个命令、要回退到本地执行时, 用户已经敲进去的输入就不会被我们吞掉。
- 没有终端的调用 (管道、文件、/dev/null) 和原来完全一样, 不分配 PTY; e2e 里「管道输出无 `\r`/NUL」两项照旧通过。

**前台判断。** 只有在调用方终端的前台进程组里, 才能改它的设置、从它读: 后台调用 tcsetattr 会触发 SIGTTOU, read 会触发 SIGTTIN。Go 用 signal.Notify 捕获了这两个信号, 系统调用会无限重启。最初的实现没有这个判断, e2e 就因此卡在 `ioctl(TCSETS)` 里空转, 直到 30 分钟超时。

- 用 tcgetpgrp 和 getpgrp 比较的办法在沙箱里不可用: 沙箱 PID 命名空间外的进程组, 两边读出来都是 0 (实测)。
- 改用内核的作业控制检查 (`pty.Foreground`): 另外 `open("/dev/tty", O_RDONLY|O_NOCTTY|O_NONBLOCK)`, 得到一个独立的 open file description, 在屏蔽 SIGTTIN 的锁定线程上对它做一次 0 字节 read。n_tty 先做作业控制检查, 后台进程组得到 EIO; 前台得到 0 或 EAGAIN。不会读走数据, 也不改调用方 fd 的 file status flags (O_NONBLOCK 只在这个新 fd 上)。
- **为什么必须是新开的非阻塞 fd**: 最初的实现直接对调用方的 fd 做阻塞的 0 字节 read, 会卡到下一次按键 (e2e 里一项卡了 70 分钟)。n_tty 的 read 一进来就拿每个终端一把的读互斥锁 (`atomic_read_lock`), 阻塞的读者在等输入的整个期间都持有它; O_NONBLOCK 的 fd 只 `mutex_trylock`, 拿不到就 EAGAIN。而我们自己的输入中转 goroutine 一直阻塞在 `read(0)` 上, 所以在中转开始之后做的判断 (退出时恢复终端前、SIGCONT 后重设 raw 前) 会排在它后面, 一直等到用户再按一个键。实测 (Linux 7.3): 与 raw 模式无关, cooked 模式下有另一个读者时同样卡住; 没有并发读者时 raw 模式也不卡。「raw 且 VMIN≥1 时 0 字节 read 阻塞」这一推断在这台内核上不成立, 触发条件是并发读者。调用方自己有线程在读终端时 (fzf 这类程序起子命令), `SetupTerminal` 的判断也会这样卡住; 新实现两种情况都不阻塞。
- 打不开 `/dev/tty` (没有控制终端) 时判为后台, 什么都不改: 这时没有作业控制可言, 也无从知道 fd 0/1/2 是哪个终端, 保守的做法是和没有终端时一样把调用方的 fd 原样交给宿主。
- 单元测试 (`internal/pty/pty_test.go`): 会话首进程是前台, 它在同一会话里新建进程组的子进程是后台; raw 终端、没有输入时不阻塞; raw 和 cooked 终端上另一个线程阻塞在读时不阻塞 (最初的实现在这两项上 2 s 超时失败); 已键入的字节判断后还在; fd flags 不变。
- 判断为后台时**不分配 PTY**, 调用方的 fd 原样交给宿主, 和原生后台作业一致: 不碰终端就照常运行, 不会被停下。这一点 e2e 有一项专门检查。
- **flatpak 自己的行为**: `flatpak run` 会把自己移到一个新的进程组 (实测 pgid = 自身 pid)。所以除非它是会话首进程, 或者由带作业控制的交互 shell 放到前台, 整个应用就是终端的后台作业。典型例子是从一个非交互脚本里 fork 出 `flatpak run`。真实场景 (VS Code 里沙箱内的交互 bash, 或者在宿主终端里直接敲 `flatpak run`) 是前台。e2e 用 `script -qec "exec …"` 让 `flatpak run` 成为会话首进程。
- **e2e 的终端驱动** (`tests/e2e/matrix.sh` 的 `on_tty`): script 的 stdin 是一个 FIFO, 写端在命令结束前一直打开, 不提供任何多余输入。最初用的是 `sleep N | script`: stdin 结束时 script 会往终端里敲一个 EOF 字符, 这个字节恰好能把阻塞在读上的中转 goroutine 叫醒, 让卡住的恢复继续下去。所以那一版 e2e 能不能过取决于时序: 命令在 EOF 字符被读走之后才结束, 就永远卡住。每个终端项最多 30 s: 超时后杀掉 script 和测试应用的实例 (`flatpak kill`), 输出 `TIMEOUT after 30s`, 该项判失败, 整个 e2e 不会挂住。另有一项「调用方已经把终端设成 raw (`stty raw -echo min 1`) 时转发」。用最初的实现跑, CLI 的两项和转发路径的六项都以 TIMEOUT 失败 (两个 runtime), 修复后全部通过。

**Ctrl-C / Ctrl-Z / 作业控制** (实测):

| 操作 | 现在的行为 |
|---|---|
| 终端上按 Ctrl-C | 原终端是 raw, `^C` 原样进入宿主 PTY, 宿主 PTY 向宿主前台进程组发 SIGINT, 宿主进程被中断。调用方看到子进程被 SIGINT 杀死 (shell 显示 130)。e2e 有一项检查 |
| 终端上按 Ctrl-Z | **没有效果**, 命令继续运行, 宿主 PTY 回显 `^Z`。原因是宿主进程是自己会话的首进程, 父进程 (session helper) 在另一个会话里, 所以它的进程组是孤儿进程组, 内核丢弃终端产生的 SIGTSTP。vim 这类自己处理 ^Z 的程序也停不下来 |
| 调用方 shell 发信号 (`kill -TSTP`/`-CONT`/`-INT` 给沙箱侧进程) | TSTP/TTIN/TTOU 以 SIGSTOP 转给宿主, 本进程随后也停下, 终端恢复原设置; CONT 恢复两边并重新设 raw; INT/TERM/HUP 等原样转发 |
| 前台作业被 `bg` 放到后台后读终端 | 拷贝输入时触发 SIGTTIN, 本进程停下 (原生程序在后台读终端也会这样); `fg` 后继续 |

Ctrl-Z 不透明, 写入已知边界。要做到透明, 得让原终端的 ISIG 跟随宿主 PTY 的设置 (宿主程序关掉 ISIG 时 ^Z 是按键, 打开时才是挂起), 这一版没有做。

## socket 代建

- `socket()`/`socketpair()` 在沙箱里返回 EAFNOSUPPORT (seccomp 按协议族拦截, 例如 AF_VSOCK) 且已启用时, C 库起 `flatpark-spawn --x-socket`。它经 HostCommand 让宿主上的自身用同样的参数创建 socket, 再用 SCM_RIGHTS 送回。要求 `SOCK_CLOEXEC` 时用 `MSG_CMSG_CLOEXEC` 接收。
- 每次调用起一个短命 helper, 不常驻。原型的测试变量 `HOSTEXEC_TEST_FORCE_SOCKET` 已删除。
- e2e 用「经过 / 绕过 preload」对比证明代建在工作: 经过 preload 时 `SO_DOMAIN=40`; `env -u LD_PRELOAD /usr/bin/python3` 得到 errno 97 (EAFNOSUPPORT)。
- 注意必须写 `/usr/bin/python3`: 写成 bare 的 `python3` 时, 已经加载 preload 的 `env` 会把 python3 本身转发到宿主 (宿主有 python3), 在宿主上自然能创建成功。原型阶段「AF_VSOCK 没被拦」的误判就是这么来的 (见文末更正)。

## 授权门控

enable.sh 先跑 `flatpark-spawn --x-probe`: 经 HostCommand 在宿主执行一次 `true`, 没有任何输出, 5 s 超时。失败时一个变量都不设置, C 库也不会被加载。应用运行期间权限被 `flatpak override` 改掉时, 已经启动的进程里转发会失败: 有本地命令就退回本地执行, 没有就报错。这一点没有专门处理。

## 名字集中

`names.mk` 是唯一定义名字的地方: 项目名、Go module、环境变量前缀、库名、扩展 id 和分支、挂载点、host-spawn 兼容名、缓存子目录、内部模式字符串。`make gen` 生成 `internal/names/names.go` (提交, 这样 `go vet`/`go test` 不依赖 make) 和 `.build/gen/names.h` (C); `runtime/enable.sh.in` 在构建时用 sed 填入。`make check-gen` 在 CI 里检查 Go 文件没有过期。manifest 和文档按字面写这些名字。

## 交付

- `packaging/org.flatpark.Spawn.yml`: 扩展 manifest, `build-extension: true`, runtime 是构建用替身 `org.flatpark.Spawn.BuildParent` (`packaging/build-parent/`, 不发布)。payload 是 extra-data, 指向 `https://github.com/flatpark/flatpark-spawn/releases/download/v<ver>/flatpark-spawn-<ver>-<arch>.tar.gz`。仓库和 release 还不存在, 所以 sha256/size 先填占位值; e2e 会把 managed block 改写成本地构建的版本。
- tarball 内容: `bin/flatpark-spawn`、`lib/libflatpark-spawn.so`、`enable.sh`、`LICENSE`。构建可复现: 排序、固定 owner 和 mtime、`gzip -n`。
- glibc 上限: `.so` 最高只能需要 **GLIBC_2.34** (实际只有 `dlsym` 需要这个版本, 其他都更老)。这样能在 freedesktop 25.08 和 26.08 两代基座上加载 (26.08 的 glibc 是 2.44)。`make lint` 和 CI 用 `scripts/check-glibc.sh` 读 readelf 检查。
- workflow: `ci.yml` 跑 check-gen、lint、test、race, 并在 x86_64 和 aarch64 原生 runner 上各构建一次 payload; `release.yml` 在推 `v*` tag 时两个架构分别构建 tarball, 再汇总成 SHA256SUMS 和 SIZES 发布 release。e2e 需要 session helper 和几 GB 的 runtime, 不进 CI。

## 覆盖矩阵

✅ 表示 e2e 在 freedesktop 26.08 和 GNOME 51 上都实测通过 (`tests/e2e/matrix.sh`)。

| 调用方/场景 | 结果 | 说明 |
|---|---|---|
| C `execvp`/`execlp`/`execve`/`posix_spawnp` | ✅ | `exec-probe` |
| Python `subprocess` (列表、`shell=True`)、`os.system` | ✅ | 逐候选 execve 按 bare name 判定 |
| Node `child_process` | ✅ | 宿主 node 经 `--filesystem=…:ro` 在沙箱里运行 |
| bash `-c`、`/bin/sh -c`、嵌套 `sh -c`、launcher 直接 exec 的 `sh -c` | ✅ | exec hook + `command_not_found_handle` |
| `env` (沙箱的 `/usr/bin/env` 和 bare `env`)、`xargs` | ✅ | |
| 只在登录 PATH 里的工具 (mise 的 `node --version`) | ✅ | 与宿主 `$SHELL -lic` 的结果一致 |
| 同名走宿主 (`cat /etc/os-release`, 三种调用方式) | ✅ | 得到宿主的 Debian |
| `/app` 参数、`sh -c` 字符串里的 `/app`、沙箱独有工具、`FLATPARK_SPAWN_LOCAL` | ✅ 留在沙箱 | |
| 宿主上测试期间新建的命令 | ✅ | 马上可用; 失败过的名字 3 s 后可用 |
| 退出码 42、命令自己的 127、真正 not found (ENOENT / FileNotFoundError / shell 127) | ✅ | |
| SIGTERM | ✅ | 调用方看到 -15, 宿主进程组消失 |
| 管道输出没有 `\r` 和 NUL | ✅ | |
| TTY 下分配 PTY、窗口大小、`-no-pty` 透传终端 | ✅ | 用 `script` 模拟 TTY, 比较设备号 |
| 转发路径 (preload exec、直接启动、command_not_found_handle) 从终端启动: 宿主进程有控制终端、`/dev/tty` 可用、窗口大小、退出码 | ✅ | |
| 转发路径, stdin 是管道、stdout 是终端 (`printf x \| prog`) | ✅ | 读到管道数据, 同时有控制终端 (由 agent 设置) |
| 转发路径, 终端的后台作业 | ✅ | 不分配 PTY, 不被停下 |
| 转发路径, Ctrl-C | ✅ | 宿主进程被中断, 130 |
| 转发路径, 调用方已把终端设成 raw | ✅ | 结束时不卡、恢复成调用方的设置 |
| 转发路径, Ctrl-Z | ❌ | 没有效果, 见「转发路径上的终端」 |
| host-spawn: shim、`-no-pty`/`-cwd`/`-env`、默认只传 TERM、`-version`、无参数开 `$SHELL`、自动 PTY | ✅ | |
| 无 `org.freedesktop.Flatpak` 权限 | ✅ 零影响 | 没有任何变量, 原生 ENOENT 和原生 bash 报错, 没有额外输出 |
| 扩展未安装 | ✅ | launcher 正常启动, 没有输出 |
| zypak `ZYPAK_LD_PRELOAD` | ✅ | 对照组: 去掉它就不生效 |
| AF_VSOCK 代建 | ✅ | 绕过 preload 时 EAFNOSUPPORT |
| 静态 Go / musl 调用方 | ❌ | 不加载 `LD_PRELOAD`; 经 `sh -c` 执行的部分照样覆盖 |
| `/app` 里的脚本、`/app` 回调 (`GIT_ASKPASS`) | ❌ | 宿主看不到; `/app` 路径翻译这一版不做 |
| zsh/fish 在沙箱里自己查 PATH | ❌ | |
| `fexecve`/`execveat` | ❌ 不拦截 | |
| Rust std 动态链接调用方 | ⚠️ 未实测 | 走 glibc 的 `posix_spawnp` 或 fork+execvp 的话应该覆盖 |
| 实时 SIGWINCH (运行中改变窗口大小) | ⚠️ 未自动化 | 代码路径: SIGWINCH 时 `TIOCSWINSZ` 到 master; e2e 只测了启动时的大小 |

## 性能

测试机: x86_64 8 核, Debian forky, Flatpak 1.18.4。`exec-probe bench` 在应用里测量 `posix_spawn` + `waitpid` 的中值, 完整 e2e 的结果 (fd / GNOME):

| 项目 | 结果 | 目标 |
|---|---|---|
| 转发 `true` (缓存命中) | 4.90 / 4.84 ms (2026-11-24 最后几轮在 4.7–5.2 ms) | ≤ 6 ms (原为 5 ms, 去掉流水线 D-Bus 连接时放宽, 见下) |
| 沙箱独有的 bare name, 第二次起 (负缓存) | 0.45 / 0.46 ms (不加载 preload 是 0.42 ms) | ≤ 3 ms |
| 两边都没有的 bare name, 第二次起 (ENOENT) | 0.05 ms | ≤ 3 ms |
| 不转发的本地 exec 增量 (绝对路径, 有/无 preload) | 58–77 µs | ≤ 100 µs |

**计时的判定**: 机器负载高时, 曾有一轮测到转发 7.9 ms、本地 exec 增量 209 µs, 整个 e2e 就被判失败。现在超过目标只报 `slow`, 不算失败; 超过目标 3 倍才算失败。负载解释不了 3 倍, 真正的回归 (每次都跑登录 shell、缓存失效) 会远超 3 倍。

转发约 4.8 ms 大致这样构成 (沙箱内 trace):
- Go 运行时启动约 1.0 ms (本机空 Go 程序 0.97 ms, C 程序 0.46 ms);
- HostCommand 约 1.5 ms (session helper fork/exec, 外加 xdg-dbus-proxy);
- 退出信号回来约 0.45 ms;
- 其余是连接、环境处理和 hook。

为此做的优化:
- 正缓存命中时直接启动, 不经 agent;
- D-Bus 连接只用 godbus 的公开 API 做了两处精简: 只提供 EXTERNAL 认证; AddMatch 不等回复 (同一连接的消息按顺序处理, match 一定先于 HostCommand 生效);
- 直接启动只发与会话不同的变量;
- 只注册需要转发的信号, 并和等待 HostCommand 回复并行;
- 为 helper 注入 `GOMAXPROCS=1` (约 0.2 ms; 带标记, helper 启动时去掉, 不会传到宿主或本地回退; 调用方自己设置的 GOMAXPROCS 不受影响);
- agent 回复后关连接之前先等它退出: 否则 xdg-dbus-proxy 会往应用的 stderr 打 GLib 警告 (实测 200 次 lookup 出现 3 次)。

**流水线 D-Bus 连接: 量过, 删了**。像 sd-bus 那样把 SASL、Hello、AddMatch、HostCommand 一次写出、不等任何回复, 需要自己编码消息。godbus 的 `Message.serial` 是未导出字段, 只能用 reflect/unsafe 写入。同机交替测量 (每组 8 轮 × 40 次的中值):

| | 流水线 | 普通 godbus | 省下 |
|---|---|---|---|
| fd 26.08 | 4.53 ms | 5.07 ms | 0.54 ms |
| GNOME 51 | 4.27 ms | 4.53 ms | 0.26 ms |

平均约 0.4 ms, 不到事先定的 0.5 ms 门槛 (省不到 0.5 ms 就不值得依赖 godbus 内部字段), 所以删掉了这个 hack, 改用普通 godbus 调用, 转发目标放宽到 6 ms。没有了对 godbus 内部字段的依赖, 升级 godbus 不会因此出问题。再往下压只能把热路径从 Go 换成 C 写的最小 D-Bus 客户端 (省约 0.5 ms 的 Go 启动), 这一版没有做。

## 测试

- `make test`: Go 单元测试。
  - `internal/routetest` 经 cgo 驱动 `preload/route.c`, 覆盖同名走宿主、私有路径、`FLATPARK_SPAWN_LOCAL`、PATH 候选、绝对/相对路径、负缓存两档 TTL 和 generation 失效;
  - `internal/agent` 覆盖登录 PATH 刷新与去抖、60 s 过期、generation、shell 输出过滤和超时, 并把 agent 作为子进程跑起来, 验证「not found 与真实 127 的区分」、环境、cwd、ENOEXEC;
  - `internal/cli` 覆盖 host-spawn 参数解析和 PTY 决策;
  - `internal/hostcmd` 覆盖退出信号的识别和 match 规则;
  - `internal/pty` 覆盖前台判断 (见「转发路径上的终端」); `internal/agent` 的 `TestAgentControllingTerminal`: 带 `--ctty` 时命令拿到 fd 5 那个 PTY 作控制终端 (`/dev/tty` 可用), 不带时没有, 带 `--ctty` 却没有 fd 5 时报错且不执行;
  - `internal/proto` 覆盖控制 socket 的大小上限: 超限的发送报错, 被截断的接收明确报「truncated」, 回复放不下时先丢掉会话指纹;
  - `internal/agent` 的 `TestLoginPathPerDirectory`: 临时 HOME 加上依赖 `$PWD` 的 `.profile` (不碰真实 dotfiles), 两个目录各自拿到各自的命令; `TestLargeMessages`: 64 KiB 的 PATH 加 60 KiB 的环境能通过, 200 KiB 的 PATH 明确报错且不执行命令;
  - `internal/sandbox` 覆盖缓存、直接启动的环境差分和环境过滤。
- `make e2e`: 见 README。与机器上其他东西隔离: 扩展、两个测试应用和构建替身都按 `org.flatpark.SpawnE2E*` 构建 (构建时由 `packaging/` 和 `tests/e2e/apps` 改写 id), 用自己的 remote `flatpark-spawn-e2e-run` 和工作目录 `.build/e2e-run`, 不碰真正的 `org.flatpark.Spawn//1` 和用它的应用 (例如本地的演示版构建 `org.flatpark.SpawnDemo.*`); 结束时比较这些 ref 和 remote `flatpark-spawn-e2e` 的前后状态, 变了就判失败; 前置检查不通过时什么都不删; 矩阵 60 项 × 2 个 runtime, 加安装、计时和扩展缺失几项。退出时 (包括失败时) 卸载所有测试 ref, 删除 remote 和宿主上的测试文件, 最后核对一遍。
- `make lint`: `go vet`、gofmt、glibc 上限、所有 shell 脚本 `sh -n`。C 用 `-Wall -Wextra -Werror` 编译。

## 原型阶段两处误判的更正

1. **AF_VSOCK 确实被拦。** 原型阶段的记录说 26.08 测试应用里 `socket(AF_VSOCK)` 直接成功、seccomp 没有拦。实际上成功是因为 preload 已经透明地在宿主上代建了 socket (原型的强制测试变量又掩盖了这一点)。复核结果: 绕过 preload 直接运行或去掉 Flatpak 权限, 都返回 EAFNOSUPPORT。本版 e2e 用「经过 / 绕过 preload」对比重新证实。另外, 绕过时必须用绝对路径 `/usr/bin/python3`, 否则 bare 的 python3 会被转发到宿主。
2. **host-spawn 的许可证是 MIT-0。** 上游 LICENSE 是 MIT 的文本, 但少了「保留版权声明」那一句, 这正是 MIT-0 与 MIT 的区别, GitHub 也识别为 MIT-0。原型阶段把它当成了标准 MIT。本版 vendor 的 PTY 代码按 MIT-0 注明来源, README 有致谢节。

## 风险与待拍板

1. apply_extra 的 runtime 固定为 fd 26.08 (`[ExtensionOf] runtime=`; 2026-11-24 决定接受)。只装了 GNOME/KDE 应用的用户, 装第一个消费方时会多拉一份 fd 26.08 Platform。
2. 消费方必须 `mkdir -p /app/lib/flatpark-spawn`, 必须不写 `autodelete: true`。建议主仓 audit 检查这两点。
3. 同名默认走宿主的影响面: 应用自己调用的 `sh`、`env`、`sed`、`cat` 都会到宿主执行, 除非参数里有私有路径。确实依赖沙箱版本的应用要用 `FLATPARK_SPAWN_LOCAL`。
4. 环境变量的过滤名单 (见上) 是经验性的。
5. 直接启动与 60 s 登录 PATH 缓存带来的滞后: 宿主切换版本后最多约 90 s 才生效。
6. 宿主进程的 argv[0] 在直接启动时是完整路径。
7. (已解决) 曾经的流水线 D-Bus 连接用 reflect/unsafe 写 godbus 的未导出字段, 已删掉 (见「性能」)。
8. 静态调用方 (Codex)、Cowork、`/app` 路径翻译、存在性检查回落都暂缓 (维护者决定)。

## 原型记录 (hostexec, 2026-11-23)

以下是原型阶段 (项目当时叫 hostexec, 组件为 `libhostexec.so` + `hostexec-run`) 的结论摘录, 保留作历史依据。判定表、性能和覆盖矩阵已被上文取代; 与上文冲突处以上文为准。

### 原型做了什么

`LD_PRELOAD` + HostCommand 原型在 freedesktop 26.08 测试应用中覆盖了 C、Node、Python、bash、`env`、`xargs`、`nohup`、`timeout`、`posix_spawnp`、绝对路径和共享目录中的 shebang; 每次 bare name 转发都跑登录 shell 取 PATH, 中值约 72 ms; `sh`/`dash` 没有兜底; 授权门控是 wrapper 的 probe。第一版保留了 exec/posix_spawn 拦截、zypak 支持、TMPDIR 共享、socket 代建、probe 门控, 删除了 host OS `access/stat/open` 回落 (`HOSTEXEC_HOST_OS_FALLBACK`) 和 `HOSTEXEC_HOST`/`HOSTEXEC_TEST_FORCE_SOCKET`。

### host-spawn v1.6.2 复用评估 (结论: vendor PTY 部分)

上游源码依赖 godbus、github.com/pkg/term、x/sys, 实现了 HostCommand/Signal/Exited、PTY、SIGWINCH; 默认在 stdout 是终端时分配 PTY, `-env` 默认只传 TERM。作为二进制依赖不合适 (入口语义是显式调用或 basename shim, 不能做 exec 判定; 默认 PTY 会改变 agent 读到的管道输出)。第一版的做法: 只 vendor `pty.go` 的思路 (约 100 行, MIT-0), D-Bus 与判定逻辑自己写, 并保留 `host-spawn` 兼容入口。当时测得 `host-spawn --no-pty /usr/bin/true` 中值约 5.47 ms。

### Cowork (暂缓)

#### 已取得的实测/静态证据

- `gh issue view 1405 -R flatpark/flatpark --comments`：维护者结论是默认包保持不支持，另做 opt-in；另一位用户报告在 host OS ro、设备和 portal 权限以及宿主 QEMU/OVMF/virtiofsd 都具备时可以运行。
- `claude-desktop_6.28899.6_amd64.deb` 下载并校验：sha256 与 manifest `7e381601...b0638cf8` 一致。
- 包含 `resources/cowork-linux-helper`（3,272,888 bytes）和 bundled `resources/virtiofsd`（2,675,512 bytes）。
- `cowork-linux-helper`：Go 1.25.14、`CGO_ENABLED=0`、静态 x86_64 ELF、无 dynamic section；`go version -m` 显示 `github.com/mdlayher/vsock v1.2.1`、`github.com/mdlayher/socket v0.4.1`。因此 preload 对 helper 无效。
- helper 字符串包含 `claude-cowork-vm.sock`、`qemuPath`、`firmwarePath`、`virtiofsdPath`、`vhost-vsock-pci`、`/dev/kvm` 相关逻辑；`--help` 显示 `-socket`，默认 `/run/user/1000/claude-cowork-vm.sock`。
- app.asar 中固定路径字符串包括 `/usr/bin/qemu-system-x86_64`、`/usr/bin/qemu-system-aarch64`、`/usr/share/OVMF/OVMF_CODE_4M.fd`、`/usr/share/OVMF/OVMF_CODE.fd`、`/usr/share/AAVMF/AAVMF_CODE.fd`、`/usr/libexec/virtiofsd`、`/usr/bin/virtiofsd`。
- 本机没有 QEMU/OVMF/virtiofsd，但有 `/dev/kvm` 和 `/dev/vhost-vsock`；没有启动 Claude GUI。

#### 是否加入 access/stat 回落

原型有 opt-in `HOSTEXEC_HOST_OS_FALLBACK=1`，只把缺失的绝对 `/usr/*`、`/etc/*`、`/opt/*` 映射到 `/run/host/...`，不默认启用。`--filesystem=host-os:ro` 下对 `/usr/bin/flatpak` 已实测 `access`、`exists`、`stat` size 和只读 open 成功；回落 access 中值约 1.49 us，普通 missing access 中值约 0.89 us。

正式实现风险很高：

- `stat/open` 会影响 loader、glibc、GIO、字体和配置探测；把 `/usr` 视为宿主会让同一进程看到混合 runtime/host 视图。
- Python/Node 内部的 `statx`、direct syscall、`openat2` 及语言 runtime 封装还需补齐；当前原型只拦 glibc stat/64 aliases/open aliases，不应声称完整。
- 只读 host OS mount 是必要但不充分；它不能让宿主看到 `/app` helper、沙箱路径的 disk image 或 app config。
- 对普通应用全局启用会误伤面远大于 Cowork 价值。建议仅在显式 `HOSTEXEC_HOST_OS_FALLBACK=1` 和 `--filesystem=host-os:ro` 的 Cowork opt-in 中启用，并且把固定路径回落限制为审核过的白名单路径；这与一般 hostexec 的“无命令名单”原则是不同的 Cowork 专用模式。

QEMU/virtiofsd 本身可由 exec host fallback 转发，且宿主进程直接打开宿主 `/dev/kvm`、`/dev/vhost-vsock`，sandbox 不需要设备 fd。静态 `cowork-linux-helper` 则不能靠 preload；除非把其 ELF fd 通过 HostCommand 以 `/proc/self/fd/N` 在宿主启动，或者应用本身提供 helper path/working directory 可配置。helper 使用 `/app/extra` 里的 VM/证书/资源路径时，整段搬到宿主执行仍会失败，必须证明所有路径在共享 TMPDIR 或宿主部署目录中相同。故当前结论是：exec/socket 技术上可拼接，但“无改应用、无路径翻译”的透明 Cowork 结论为不支持。

### 反向回调

`GIT_ASKPASS`、`GIT_EDITOR`、credential helper 可能指向 `/app/...`：宿主 git 能执行，但回调路径对宿主不可见。建议按优先级：

1. 应用 wrapper 把 hostexec 专用回调放在共享 `/run/user/$UID/app/$FLATPAK_ID/hostexec-callbacks/`，host git 看到同一路径；
2. 对已知 callback，生成小的沙箱端 Unix socket proxy，宿主侧 callback 用 HostCommand fd 映射或共享 socket 调回；协议只传 stdin/stdout，不把任意命令作为参数；
3. 不要把 `/app` 整树 host mount，也不要让宿主 git直接执行任意 app 路径。

若 callback 是厂商二进制硬编码 `/app`，在“不改应用”约束下不能保证；应把该场景标为不支持。

### 原型之后的补充实验 (2026-11-24)

基准工具 `tests/hostcommand-bench/` (宿主上直接跑, HostCommand 行为与沙箱内一致; 本机 Flatpak 1.18.4)。

#### PATH 解析

| 方式 | 中值 | 找不到命令时 |
|---|---|---|
| HostCommand 直接给 bare name | 0.89 ms | spawn 前就返回 D-Bus 错误 `Failed to execute child process “x” (No such file or directory)`, 无副作用, 与真实 exit 127 可区分 |
| bare name + `envs` 里传 PATH | 同上 | **`envs` 的 PATH 不参与查找**: `PATH=/nonexistent` 照样找到 `/usr/bin/true`, 传了 mise 目录也找不到 `gh` |
| `/usr/bin/env name` + `envs` PATH | 1.35 ms | exit 127 + stderr, 与命令自身 127 混淆 |
| 每次 `$SHELL -lic` 取 PATH (原型现状) | 72 ms | — |
| HostCommand 执行自身 ELF (`/proc/self/fd/4`, 4.4 MB 静态 Go) | 2.0 ms | — |

session-helper 自己的 PATH 来自 systemd user 环境 (本机 `~/.local/bin:/usr/local/bin:/usr/bin:/bin:...`), 登录 shell 的 PATH 在前面多了全部 mise 安装目录 (node/go/gh/claude…)。所以「直接 bare name」有两个问题: mise 类工具找不到; 同名命令时优先级与用户终端不一致 (例如拿到 `/usr/bin/node` 而不是 mise node)。

建议正式方案: **宿主侧 agent = hostexec-run 自身经 fd 送到宿主执行** (已被 socket 代建验证过的同一手法), 每次转发约 2 ms:

- agent 在宿主上按缓存的 login PATH 做 `LookPath`, 然后 `execve`; 缓存放宿主 `$XDG_RUNTIME_DIR/hostexec/path` (宿主路径, 跨应用共享, 重启即清)。
- 查找失败时 agent 现场重跑一次 `$SHELL -lic` 刷新缓存再查 (只在 miss 时付 70 ms), 所以宿主新装的命令立即可用。
- 仍找不到时, agent 通过控制 fd 告诉沙箱侧「not found」, 沙箱侧返回原始 ENOENT / 127, 不与命令自身的 127 混淆。
- 优先级与用户终端一致, 不再依赖 session-helper 的 PATH。

#### sh / posix 模式兜底

runtime (freedesktop 26.08) 里 `/bin/sh` → bash 5.3, 没有 dash。实测把 hook 放进环境变量 `BASH_FUNC_command_not_found_handle%%='() { ...; }'`:

- `sh -c 'nosuchcmd'` (posix 模式 on) 调用了 hook;
- bash、bash 里嵌套的 `sh -c`、`/bin/sh -c` 都继承并调用。

结论: **用导出函数取代 `BASH_ENV`**。sh 缺口补上了, 也不再和应用自己的 `BASH_ENV` 冲突 (`HOSTEXEC_ORIGINAL_BASH_ENV` 链式 source 可删)。剩余缺口是 zsh/fish 等非 bash shell 在沙箱里自己查 PATH (runtime 默认不带), 以及 `env -i`、bash `-p`/setuid 这类场景, 这些场景下 hook 和 `LD_PRELOAD` 本来就一起失效。

#### `/app` 路径翻译

沙箱内 `/.flatpak-info` 的 `[Instance] app-path=` 就是 `/app` 在宿主上的真实部署目录 (含 `extra/`), 宿主用户可读 (已在 `ai.usemagpie.Magpie` 上确认)。`runtime-path=` 同理对应 `/usr`。所以:

- 放在 `/app` 里、解释器只在宿主上有的 shebang 脚本 (原 ❌), 可以把 `/app/...` 换成 `${app-path}/...` 再转发;
- 静态二进制 (Cowork `cowork-linux-helper` 这类 Go 静态 ELF) 两条路都行: fd 执行, 或者翻译路径后执行; 翻译路径的好处是 `/proc/self/exe` 和相对资源路径在宿主上仍然有效;
- 动态链接的 `/app` 二进制不能翻译 (宿主上的 ld.so 和库不对), 判定表里应保持留在沙箱。
- deploy 目录带 commit hash; flatpak 对运行中实例的旧 deploy 会延后到实例退出才删 (未在本机复测), 所以翻译出来的路径在进程生命周期内应当稳定。

翻译只作用于 argv[0]/脚本路径, 不改写参数里的其他 `/app` 路径, 以免误伤。

### 维护者决定 (2026-11-24)

- 第一版核心目标: 沙箱内应用能调用宿主工具链 (mise/系统命令), 其余是加分项。
- PATH: fd 送宿主 agent + 宿主侧 PATH 缓存 + miss 时刷新 (见上节)。
- sh 兜底: 导出函数 `BASH_FUNC_command_not_found_handle%%` 取代 `BASH_ENV`。
- 同名命令: **默认走宿主**。沙箱和宿主都有的 bare name 转发到宿主; 只有宿主没有时才留在沙箱。
- `/app` 路径翻译: 暂不做。
- Codex / Cowork (静态调用方、存在性回落): 暂缓, 第一版不做。
- 命名 / host-spawn 复用: 当时待定, 后来定为改名 flatpark-spawn、只借 PTY 部分 (host-spawn v1.6.2 全部源码约 9.4 KB, 真正要借的是 `pty.go` 约 100 行)。


## 升级 freedesktop runtime (例如 26.08 → 27.08)

扩展 payload 与 runtime 无关 (静态 Go + glibc 符号上限 2.34 的 `.so`), 分支 `1` 是 ABI 版本不是 runtime 版本, 消费方换 runtime 时不用动扩展。真正绑 runtime 的只有扩展安装时 apply_extra 用的 runtime:

1. 改 `packaging/build-parent/org.flatpark.Spawn.BuildParent.yml` 的 `runtime-version` 和 `packaging/org.flatpark.Spawn.yml` 的 `sdk`; e2e 测试应用 (`tests/e2e/apps/`) 换到新 runtime / 对应 GNOME 版本。
2. `make e2e`。每次升级重点看: runtime 的 `/bin/sh` 仍是 bash (导出函数兜底依赖它)、Electron BaseApp 的 zypak 仍支持 `ZYPAK_LD_PRELOAD`、seccomp 对 AF_VSOCK 的行为 (socket 代建)。
3. 重建扩展发新 commit, 分支仍为 `1`。在旧 runtime EOL 前完成, 免得新用户安装时还要拉 EOL 的 runtime。
