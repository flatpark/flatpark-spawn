# flatpark-spawn 规格书 (原名 hostexec)

> 2026-11-24 起改名 flatpark-spawn, 交付形式改为共享扩展 `org.flatpark.Spawn`; 下文与此冲突处以第 11 节决策记录为准。

状态: 第一版已实现 (2026-11-24), 设计、实测和覆盖矩阵见 `DESIGN.md`。本文是最初的需求与安全模型 (2026-11-23), 保留作依据。
本文汇总 2026-11-23 的讨论结论, 以及 FlatPark 仓库现状的盘点。
`DESIGN.md` 用实测推翻了这里的某条结论时, 以 `DESIGN.md` 为准; 之后的决定记在第 11 节。

---

## 1. 背景

### 1.1 起因

- **flatpark#1523**: Claude Desktop 的 Code 功能显示不了 diff, 因为 Freedesktop Platform 运行时里没有 git。
- **短期修复**: PR #1526 已于 2026-11-23 合并, 做了三件事:
  - Claude Desktop 和 ChatGPT 的运行时改成 `org.freedesktop.Sdk`, Sdk 自带 git、ssh、python3、make;
  - 加了 `~/.gitconfig:ro`、`xdg-config/git:ro`、`--socket=ssh-auth`;
  - ChatGPT 删掉了它的 `git-host` shim 和 `--talk-name=org.freedesktop.Flatpak`。
- **已验证**: Flatpak 授予 `xdg-config/git` 时, 会把它同时挂到应用重定向后的 `XDG_CONFIG_HOME` 下面。
- **短期方案的代价**: 宿主上的 credential helper (gh、libsecret) 在沙箱里调不到。
- 维护者的决定: 先用 Sdk 顶着, 等 hostexec 做出来、验证没问题后, 再把这两个包切过去。

### 1.2 根本问题

AI 和编程类应用里的 agent 会执行任意命令, 比如 mise、node、cargo、gh、docker、kubectl, 以及用户自己装的各种 CLI。Sdk 只能提供一套基础工具链, 用户在宿主上装的工具版本、配置和登录状态都进不了沙箱。

现在大约 20 个包各自写了一份 `flatpak-spawn --host` 桥 (盘点见第 9 节), 普遍有下面几个问题:
- 每个包都复制粘贴一份, 修一个 bug 要改很多处;
- 按命令名一个个包装: 启动时枚举宿主 PATH, 给每个命令建一个 symlink;
- 宿主上新装的命令, 要重启应用才能看到;
- 拦不到用绝对路径调用的命令。

---

## 2. 目标与安全模型

### 2.1 维护者的硬需求

1. **应用本身不改。** 厂商的二进制原样运行。我们能改的只有 manifest、wrapper (加一两行) 和我们自己提供的文件。
2. **全自动发现, 不按命令包装。** 代码里不能有命令名单, 也不能生成 symlink 目录。比如应用调用 `mise`: 沙箱里没有、宿主上有, 就在宿主上执行。宿主上新装的命令立刻就能调用。
3. **依赖不能崩。** 转发出去的命令在宿主上原生运行, 它派生的子进程也留在宿主上。退出码、stdio、tty、信号对调用方来说都要是透明的。
4. **不改 flatpak**, 也不要求用户在宿主上额外安装任何东西。
5. **没有授权时完全无害。** 应用没拿到 `org.freedesktop.Flatpak` 权限时, hostexec 的行为要和没装它一样。

### 2.2 安全模型

hostexec 本质上就是让应用跑出沙箱, 这是有意为之。维护者接受这个代价, 前提是: 这层由 FlatPark 自己开发、维护, 而且能用它的应用都经过审计。具体是:

- **授权点在应用的 manifest。** 扩展或模块本身拿不到任何权限, 应用必须自己声明 `--talk-name=org.freedesktop.Flatpak`。
- **CI 卡白名单。** 一个应用如果引用了 hostexec, 必须同时满足三条: 声明了 `policy.dangerous_permissions`、写了 `packaging: kind: host-command`、app id 在白名单里。
- **用户可以关掉。** metainfo 里给出关闭命令: `flatpak override --user --no-talk-name=org.freedesktop.Flatpak <id>`。
- **不设计"安全子集"。** 目标是透明、可靠。命令白名单之类的限制没有安全意义, 因为 `node -e` 和 git hooks 本来就能执行任意代码。

---

## 3. 命名

用 **`hostexec`**, 名字直接说明 exec 会去宿主执行, 审计时一眼能看出它跑出沙箱。名字在代码里集中定义在一处。

排除的候选:

| 名字 | 排除原因 |
|---|---|
| `devtools` | 听起来像"一包开发工具"; 和 Chrome DevTools、Sdk 扩展容易混; 看不出它的危险属性 |
| `host-spawn` | 已经是 1player/host-spawn 项目的名字, FlatPark 的 VS Code 包也在用它, 会撞名。flatpak 自己的写法是 `flatpak-spawn --host`, 这个名字并不更"官方" |
| `hostpass` | 容易读成 host password |
| `hostbridge` | 可以接受但太泛, 留作备选 |
| `passthru` | 看不出往哪儿透传 |

---

## 4. 架构 (初始设计, 由原型实测确认)

拦截点放在 **exec 层**, 不放在 PATH 层。

### 4.1 组件

| 组件 | 形态 | 职责 |
|---|---|---|
| `libhostexec.so` | 小型 C 库, 通过 `LD_PRELOAD` 注入, 与 zypak 的 preload 共存 | 拦截 exec、socket、文件访问, 判定哪些操作要交给宿主 |
| `hostrun` | Go 静态单文件 (优先评估复用 1player/host-spawn, 见 4.6) | 通过 D-Bus 调用 `org.freedesktop.Flatpak.Development.HostCommand`; 转发 fd、信号、退出码和 pty; 清洗环境变量 |
| 宿主侧 helper | 由 HostCommand 拉起 | 在宿主上代建 socket, 把 fd 递回沙箱 |
| shell 兜底 | 通过 `BASH_ENV` / `ENV` 注入 `command_not_found_handle` | bash 自己用 stat 查 PATH, 不经过 exec, 需要单独兜底 |
| 偏好配置 | 很小的配置文件或环境变量 | 解决沙箱和宿主同名命令的冲突 (python3、ssh、git 等)。它只做冲突裁决, 不算命令包装 |

### 4.2 exec 判定规则

需要拦截整个 exec 家族: execve、execv、execvp、execvpe、execl*、execveat、fexecve、posix_spawn、posix_spawnp。原因是 glibc 的 execvp 内部调用的是 `__execve`, 不经过 PLT, 只拦 execve 不够。

| 情况 | 处理 |
|---|---|
| 按名字查找, 在沙箱 PATH 里找到了 | 在沙箱里执行 (`sh`、coreutils、应用内部的脚本都必须留在沙箱) |
| 按名字查找, 沙箱里报 ENOENT | 转成 `hostrun <name> args...`, 由宿主按自己的 PATH 解析 (包括 login shell 的 PATH, 这样 mise、nvm 装的命令也能找到) |
| 绝对路径, 沙箱里不存在、宿主上存在 | 转发到宿主, 例如 `/usr/bin/docker`、`~/.local/bin/mise` |
| 绝对路径, 位于 `/app/...`, 且被偏好标记为"在宿主上跑" | 先做路径翻译 (见 4.5), 再转发 |
| 沙箱和宿主都有的命令 | 按偏好配置决定, 默认走沙箱 |

`hostrun` 是 exec 进当前进程的, 所以调用方看到的 PID 和 stdio 都不变, 察觉不到中间有一层。命令转到宿主后, 它派生的整棵进程树都在宿主上原生运行。

### 4.3 跨越边界时的环境处理

- **环境变量**: 去掉 `LD_PRELOAD`、`ZYPAK_*`; 把 `XDG_*_HOME` 还原成宿主值; PATH 换成宿主的。
- **cwd**: 必须是宿主上存在的目录, 不存在就回退。现有 shell 版 shim 里的踩坑注释要逐条吸收。
- **TMPDIR**: 指向 `$XDG_RUNTIME_DIR/app/$FLATPAK_ID`, 这个路径在沙箱内外相同。
- **login shell 解析 PATH 时**: shell 启动输出不能混进 stdout, 因为 agent 会解析这部分输出。

### 4.4 socket 代建 (2026-11-23 已实测)

- **现象**: 沙箱里 `socket(AF_VSOCK)` 被 Flatpak 的 seccomp 拦下, 返回 EAFNOSUPPORT, AF_PACKET 也一样。宿主上正常。
- **实测**: 在宿主上创建 AF_VSOCK socket, 用 SCM_RIGHTS 把 fd 传进沙箱 (org.flatpak.Builder)。沙箱里拿到的 family 是 40 (AF_VSOCK), connect 到宿主上监听的 vsock 端口后, 收到了 `hello-from-host-vsock`。
- **结论**: seccomp 只在**创建** socket 那一步按协议族拦截, 已经存在的 fd 照常可用。代建出来的是真正的 socket, 不需要转发字节, 没有拷贝开销。
- **设计**:
  - preload 拦截 `socket()` / `socketpair()`, 沙箱里返回 EAFNOSUPPORT 时, 改由宿主 helper 用相同参数创建, 再把 fd 递回来;
  - 不维护协议族名单, 只有被拦了才走这条路;
  - 待定: 宿主 helper 要不要常驻复用, 接口应该收得多窄。
- **不做的事**: 不通过改写 `bind`/`connect` 路径来处理文件路径型 unix socket, 因为另一端常常是从 argv 拿到路径的, 只改一边就对不上。改为约定路径: TMPDIR 放在共享目录; 如果应用把 `/tmp` 写死了, 就在 opt-in 配置里 grant `/tmp`。
- **不需要处理**: TCP/UDP 和 abstract unix socket。只要授予了 `--share=network`, 就和宿主共用同一个网络命名空间, 本来就是通的。

### 4.5 `/app` 路径翻译 (正式能力, 待设计)

Ventoy、Cowork、Positron 这类场景, 需要把应用自带的二进制放到宿主上执行。宿主上没有 `/app` 这个路径, 要翻译成对应的部署目录, 例如 `~/.local/share/flatpak/app/<id>/<arch>/<branch>/active/files/...`, system 安装则在 `/var/lib/flatpak/...` 下。

需要调研的问题:
- extra-data 解出来后在部署目录里的实际位置;
- user 安装和 system 安装分别是什么路径;
- 二进制在宿主上运行时依赖的是宿主的库, 等同于原生 tarball 安装的效果;
- 不能对部署目录下的文件做 setcap、改 xattr: user 安装时这些文件和 ostree repo 对象是硬链接, 改了会把仓库对象一起改坏。

### 4.6 复用 1player/host-spawn

https://github.com/1player/host-spawn 基本信息: MIT-0 许可, Go 静态单文件, v1.6.2。它是 `flatpak-spawn --host` 的重写, 已经处理了 pty、SIGWINCH, 并透传 `$TERM` 和 `$PWD`。FlatPark 的 VS Code 包通过 extra-data 带了它。

- **它能替代的部分**: `hostrun`。
- **它替代不了的部分**: 全自动发现。它的 shim 用法同样是一个命令一个 symlink。
- **待实测**:
  - 环境清洗的能力够不够;
  - 默认分配的 pty 会不会污染 agent 通过管道读到的输出, 比如多出 `\r`、颜色控制码。
- 结论只能是依赖、vendor 或 fork 三选一。

### 4.7 存在性检查回落 (可选能力, 由 Cowork 场景驱动)

有些应用会用 `fs.access`/`stat` 去检查固定路径, 比如 `/usr/bin/qemu-system-*`。设想的做法是 preload 也拦截 access/faccessat/stat 系列和只读的 open: 路径在沙箱里不存在时, 回落去查 `--filesystem=host-os:ro` 挂进来的 `/run/host/...`。

这个能力误伤面很大, 必须评估清楚: 库加载、配置文件探测、运行时对 `/usr` 的各种假设都可能受影响。比较可能的结果是默认关闭, 由应用显式开启。

---

## 5. 能力边界: 三档

hostexec 只能把应用带到**宿主上的普通用户**层级, 它本身不提供提权。

| 档 | 例子 | 能否做到原生体验 | 需要的能力 |
|---|---|---|---|
| **1 用户级** | AI/IDE 的 agent 工具链、SSH 管理器调用 `ssh`、git、mise、docker CLI、kubectl 认证插件 | ✅ | exec 转发 |
| **2 运行时 pkexec** | Ventoy (#1483)、写盘工具、Cowork 的 QEMU (#1405)、Positron 的 ark 内核 (#451) | ✅, 但运行时没有沙箱, 而且是 root | exec 转发 + `/app` 路径翻译; 宿主上的 polkit 负责弹认证框 |
| **3 安装时提权** | tun 类 VPN/代理、systemd 系统服务、udev 规则、内核模块、setcap | ❌ 不建议做 | 原生安装靠 postinst 以 root 执行一次完成这些, Flatpak 没有这个环节 |

第 3 档的几种绕法和它们的问题:

- **每次运行都用 pkexec 启动核心**: 能用, 但每次都要输密码。Drosophila 现在就是这么做的, 它通过 `flatpak-spawn` 加 pkexec, 把权限限制在 CAP_NET_ADMIN。
- **首次运行时用 pkexec 往宿主装服务**:
  - 这等于偷偷做了一次宿主安装, `flatpak uninstall` 后会有残留;
  - setcap 不能直接打在部署目录的文件上 (原因见 4.5)。
- **由特权 helper 打开 tun 后把 fd 递进来** (Android VpnService 模型): 技术上可行, 但 mihomo 这类核心是自己用 ioctl 和 netlink 配置 tun、路由、DNS 的, 没法通用地透明拦截, 只能逐个应用适配。

第 3 档不归 hostexec 管。这类应用要么继续判定为不适合 Flatpak, 要么只提供不需要提权的模式。

hostexec 只处理"沙箱 → 宿主"这个方向。下面这些是"宿主 → 沙箱"方向或者桌面集成问题, 不在它的范围内:
- 浏览器的 Native Messaging: 宿主上的浏览器要拉起沙箱里的 helper;
- 开机自启;
- Wayland 全局快捷键;
- 文件管理器扩展。

---

## 6. 高难度验收场景: Claude Desktop 的 Cowork (#1405)

Cowork 依赖宿主的四个地方, 以及对应的解法:

| 卡点 | 解法 | 状态 |
|---|---|---|
| Electron 主进程用 `fs.access` 检查 QEMU、OVMF、virtiofsd 的固定路径 | 4.7 存在性检查回落 | 待调研 |
| `cowork-linux-helper` 用绝对路径启动 QEMU 和 virtiofsd | 4.2 exec 转发。QEMU 在宿主上自己打开 `/dev/kvm`, 沙箱不需要 `--device=all` | 待实测 |
| 通过 vsock 和 VM 通信 | 4.4 socket 代建 | 机制已实测 |
| `cowork-linux-helper` 如果是静态链接, preload 无效 | 把整个 helper 放到宿主上跑, 依赖 4.5 路径翻译 | 待分析 .deb |

另外还要确认 socket 和磁盘镜像文件放在哪里, 沙箱内外路径是否一致。

参考: issue 报告人 ajithakdev 说他已经在 FlatPark 包上跑通了 Cowork, 用的是设备权限、`host-os:ro` 和 Flatpak portal, 没改应用。我们请他发 PR 并说明他是怎么绕过 `/usr` 固定路径检查的, 截至 2026-11-23 还没收到 PR。

---

## 7. 交付与接入

- **交付形式**: 先做成 `flatpark/prebuilt` 里的一个 stack, 由各应用在构建时引用。这样沿用现有流水线, 不需要新的基础设施。等稳定下来, 再考虑做成多个父应用共用的 Flatpak 扩展 (`org.flatpark.<Name>`), 好处是一次更新所有应用同时生效。现在的 extension 流水线只支持"一个父应用"。
- **消费方需要加的东西**, 目标是几行:
  - manifest: 引用 prebuilt 模块; 加 `--talk-name=org.freedesktop.Flatpak`; 在 flatpark.yml 里补 policy 和 packaging 两项;
  - wrapper: 加一行 `. /app/lib/hostexec/env`, 由它设置 `LD_PRELOAD`、`BASH_ENV`、TMPDIR。
- **运行时无关**: C 库只依赖较老的 glibc 符号版本, 要能在 freedesktop 26.08、GNOME、KDE 运行时上加载。
- **文档**: `flatpark/docs/packagers/` 新增一页, 写清约定、边界、三档能力和审计规则; `permissions.md` 中 host-command 相关的部分要更新。
- **CI**: 白名单规则见 2.2。

---

## 8. 已知边界 (预期 ❌ 或 ⚠️, 原型阶段确认)

- **静态链接的调用方** (Go、musl 的 Rust): LD_PRELOAD 对它们无效。ChatGPT 里的 Codex 可能就是 musl 静态构建。这类程序通过 `bash -c` 执行的命令仍能被覆盖。
- **宿主反过来调沙箱里的东西**: `GIT_ASKPASS`、`GIT_EDITOR`、credential helper 指向 `/app/...` 时, 宿主上执行不了。
- **参数里带只在沙箱里存在的路径**: `/app/...`、`~/.var/app` 之外未授权的路径。
- **性能**: 每次转发多一次 HostCommand 往返, 大约几十毫秒, 实际数值待测。agent 会频繁调用 `git status`。

---

## 9. 仓库盘点 (2026-11-23, registry 共 370 个包)

### 9.1 已有宿主桥的包 (授予了 `--talk-name=org.freedesktop.Flatpak`, 共 20 个)

hostexec 做出来后, 这批是迁移对象: 能统一实现的统一掉, 删掉自写的脚本。

| 包 | 现在的桥 | 自写脚本 | 迁移价值 |
|---|---|---|---|
| `co.anysphere.cursor` | agent 和编辑器的命令桥, 启动时生成 symlink 目录 | `cursor-host-shim` `cursor-setup-host-bin` | 高, 正是 hostexec 要替代的模式 |
| `ai.z.zcode` | 同上 | `zcode-host-shim` `zcode-setup-host-bin` | 高 |
| `com.tencent.codebuddycn` | 同上 | `codebuddy-host-shim` `codebuddy-setup-host-bin` | 高 |
| `io.github.abcwyc.pi-agent-desktop` | 同上 | `pi-agent-host-shim` `pi-agent-setup-host-bin` | 高 |
| `com.chipichapa.sele` | 宿主上的 coding agent CLI、git、shell | (写在 manifest 里) | 高 |
| `com.aptakube.Aptakube` | kubectl 和各家云厂商的认证插件 (aws、az、gcloud 等) | `host-shim` | 高, 认证插件是绝对路径或按名字调用, 正好适用 |
| `io.github.MarcoDotIO.Ycode` | swift、git、lldb 等工具, 上游自带启动器 | (上游自带) | 中, 要看上游的启动器 |
| `io.enpass.Enpass` | lsof、readlink、cat 三个命令 | `lsof-host` `readlink-host` `cat-host` | 中。注意 `cat` 和 `readlink` 沙箱里也有, 需要偏好配置强制走宿主 |
| `com.visualstudio.code` | 自带 host-spawn, 终端可以手动指向它 | (extra-data 里的 host-spawn) | 中, 能让终端和扩展透明拿到宿主工具链 |
| `app.voltius.Voltius` | 本地终端打开宿主的 login shell | (manifest) | 低, 显式调用 shell 已经够用, 统一实现即可 |
| `com.kang.nyaterm` | 本地终端 | `nyaterm-host-shell` | 低, 同上 |
| `org.tunaos.blueshell` | 终端、容器会话 | (manifest) | 低, 同上 |
| `org.tunaos.compass` | 启动器运行宿主应用 | (manifest) | 低 |
| `org.tunaos.tavern` | 调用宿主的 brew | (manifest) | 中, brew 是按名字调用的 |
| `io.github.killown.flux` | git、7z、ffmpeg 走宿主的 `/usr/bin`, 沿用上游的宽权限 | (上游) | 中 |
| `io.github.ClementTsang.bottom` | 整个 bottom 二进制在宿主上运行 | (wrapper) | 低, 属于"整体放到宿主跑"模式 |
| `io.github.antux912012.Mithrandir` | 所有系统管理操作在宿主执行, 含 sudo 和 pkexec | (manifest) | 第 2 档样本 |
| `io.github.ergolyam.Drosophila` | TUN 模式下用 pkexec 以 CAP_NET_ADMIN 在宿主上运行自身 | (上游) | 第 3 档的现成绕法样本, 不迁移 |
| `org.gnome.Software` | flatpak run、rpm-ostree、fusermount 等 | (上游) | 不迁移, 它是系统管理工具 |
| `ai.storyteller.wordcraft` | 注释说上游的更新器需要它, 但 FlatPark 不需要那个更新器 | — | **需要复查**: 既然用不到, 为什么还授予了这个权限 |

### 9.2 因为沙箱被降级、hostexec 可能改善的包 (摘自 metainfo)

**第 1 档: exec 转发就能补上**

- `ai.usemagpie.Magpie`: 跑不了宿主上的 agent CLI, 也看不到 agent 是否在运行。
- `com.ollama.Ollama`: 不支持安装或启动宿主上的 coding 工具。
- `org.rustgis.RustGIS`: 不能把任务交给宿主上的 coding agent。
- `io.github.simonschubert.Kai`: shell 工具只能用运行时自带的命令。
- `com.tencent.WorkBuddy` / `com.tencent.WorkBuddyAI`: 开启内置沙箱后命令工具无法启动, 需要具体分析。
- `io.github.lyricat.Inua`: Color Picker、Pass、Tailscale 依赖宿主命令。
- `io.github.jlcodes99.CockpitTools`: 读取宿主 keyring 密钥需要宿主上的 helper。
- `com.cameronamer.telegramdrive`: 预览要用宿主的 FFmpeg。
- `org.openaec.Open3DViewer`: 渲染要用宿主的 Blender。
- `org.openaec.OpenOfficeStudio`: 转换旧格式要用宿主的 LibreOffice。
- `com.cyanvoxel.tagstudio`: ripgrep 加速和 RAR 解压。
- `io.github.tim_van_oudheusden.Dailies`: 在沙箱外打开指定浏览器。
- `org.nectarlink.desktop`: 在文件管理器里浏览手机存储需要宿主的 FUSE。fusermount 在宿主执行可能可行, 待验证。
- `com.anthropic.ClaudeDesktop`、`com.openai.ChatGPT`: 宿主上的 credential helper, 以及完整的宿主工具链。

**第 2 档: 运行时 pkexec 或"整体放到宿主跑", 需要 `/app` 路径翻译**

- `io.github.xiaokentrl.phpo`: 写 `/etc/hosts` 需要 pkexec。
- `com.anthropic.ClaudeDesktop` 的 Cowork (#1405)。

**收益有限, 需要逐个评估**

- `io.github.ggml_org.LlamaCpp.*`、`io.github.EricLBuehler.MistralRs.CPU`: 都写了"不包含宿主工具链和 Python"。
- `org.comfy.ComfyDesktop`: 需要宿主编译器或 CUDA toolkit 的自定义节点。在宿主上编译出来的产物要回到沙箱里加载, ABI 可能对不上。
- `app.geolibre.GeoLibre`: 缺 Python 地理库, 更可能的解法是打包进去。

**hostexec 管不了 (反方向或桌面集成问题)**

- Native Messaging: `com.fluxdown.app`、`com.misterdownloadmanager.MisterDownloadManager`、`dev.aninsomniacy.rayburst`。
- 开机自启: Magpie、ccswitch、MarkShot、DashBeam、nectarlink。
- Wayland 全局快捷键和文本注入: voquill、WritingTools、openai-translator。
- `br.gov.serpro.desktop.assinador`、`br.jus.cnj.PJeOfficePro`: A3 令牌驱动需要在宿主上做一次性安装。
- `io.github.vallierdev.UURemoteViewer` 的被控端: 需要 PAM 和 systemd 用户服务, 属于第 3 档。
- `me.ahmetcetinkaya.whph`: 需要知道当前焦点窗口。

### 9.3 因为沙箱限制被搁置或拒绝的应用请求

| issue | 应用 | 卡点 | hostexec 之后 |
|---|---|---|---|
| #1483 | Ventoy | 需要 root 写块设备; 上游 Linux GUI 自己会用 pkexec 提权 | 第 2 档: exec 转发 pkexec, 加 `/app` 路径翻译, 体验和原生一致。UDisks2 方案更干净, 但需要上游改代码 (有实验性的 VentoyUDisks fork) |
| #451 | Positron (not planned) | R 内核 ark 要加载宿主的 libR.so, 它依赖的宿主库在沙箱里看不到 | 第 2 档 (不需要 root): ark 整体放到宿主上运行, 用 `/app` 路径翻译; 内核通信走 ZeroMQ/TCP, 网络命名空间是共享的。值得重新评估 |
| #1405 | Claude Desktop Cowork | 见第 6 节 | 高难度验收场景 |
| #403 | Vocalinux | ydotool、xdotool、wl-clipboard、xsel 不在运行时里 | 第 1 档: 用宿主上的这些工具 (ydotool 还需要宿主上的 ydotoold 和 uinput 权限)。也可以按原计划打包进 prebuilt |
| #1351 | Drosophila (已收录) | TUN 需要 CAP_NET_ADMIN | 第 3 档样本, 已经用 pkexec 绕过, 不迁移 |

---

## 10. 阶段计划

1. **原型 (已完成, 2026-11-23)**: 调研、做原型、实测各个边界 (含复用 host-spawn、socket 代建、Cowork 的评估), 结论见 `DESIGN.md`「原型记录」。
2. **第一版 (已实现, 2026-11-24)**: 正式实现。`make test` 跑单元测试, `make e2e` 在测试 Flatpak 里跑覆盖矩阵。交付形式按第 11 节改为共享扩展 `org.flatpark.Spawn`, 不再出 prebuilt manifest。
3. **试点**: 在 Claude Desktop 和 ChatGPT 上试点, 替换掉 #1526 的纯 Sdk 方案, 恢复宿主上的 credential helper。
4. **迁移**: 迁移 9.1 中的高、中价值包, 删掉它们自写的 shim。
5. **第 2 档能力**: 用 9.2 和 9.3 里的样本验证第 2 档能力 (Ventoy、Positron、Cowork), 写文档, 加 CI 白名单。

## 11. 决策记录

| 日期 | 决策 |
|---|---|
| 2026-11-23 | 安全模型: 允许完全跑出沙箱, 依靠应用审计、manifest 显式授权和 CI 白名单来控制 |
| 2026-11-23 | 不按命令包装, 在 exec 层做全自动发现 |
| 2026-11-23 | 短期方案: Claude Desktop 和 ChatGPT 改用 Sdk (#1526, 已合并), hostexec 验证通过后再切换 |
| 2026-11-23 | 名字定为 `hostexec`, 不用 devtools 或 host-spawn |
| 2026-11-23 | socket 代建纳入 hostexec 的能力 (vsock 实测通过) |
| 2026-11-23 | 第 3 档 (安装时提权) 不纳入 hostexec 的范围 |
| 2026-11-23 | 交付形式先做 prebuilt module, 之后再考虑做成多父应用共用的扩展 |
| 2026-11-24 | 改名 **flatpark-spawn** (取代 hostexec 的决定); README 注明 PTY 代码与思路来自 1player/host-spawn (MIT-0) |
| 2026-11-24 | 交付形式改为**直接做成多应用共用的 Flatpak 扩展 `org.flatpark.Spawn`** (取代上一条 prebuilt 先行); 项目里第一个这种类型的扩展 |
| 2026-11-24 | 第一版核心目标: 沙箱内调用宿主工具链。`/app` 路径翻译、存在性检查回落、Codex/Cowork 暂缓 |
| 2026-11-24 | 同名命令默认走宿主; 参数引用沙箱私有路径 (如 `/app/`) 时留在沙箱; 宿主没有的命令负缓存 |
| 2026-11-24 | PATH: hostexec-run 经 fd 送宿主当 agent, 宿主侧缓存 login PATH, miss 时刷新; sh 兜底改用导出函数 `BASH_FUNC_command_not_found_handle%%`, 不再用 BASH_ENV |
| 2026-11-24 | 之后由 flatpark-spawn 替换 VS Code 包里上游 host-spawn, 保留 `host-spawn` 兼容入口 |
| 2026-11-24 | 扩展 apply_extra 固定用 freedesktop 26.08 Platform, 接受 (消费方应用本来就会带) |
| 2026-11-24 | 同名走宿主的沙箱私有路径名单 (`/app`、`/usr` 系、`/tmp`、`/var/tmp`、flatpak 布局、`/dev/fd`; `/etc` 不算) 接受 |
| 2026-11-24 | 项目 LICENSE 用 MIT (同 rpak); 扩展内自带 `bin/host-spawn` 符号链接 |

## 12. 待拍板

- 4.7 存在性检查回落: 是否纳入? 默认开还是由应用显式开启?
- 偏好配置的形态: 放 manifest 里的环境变量, 还是用配置文件? 用户能否自行覆盖?
- `/app` 路径翻译: 是否要求应用显式声明哪些二进制允许在宿主上运行?
- 第 2 档 (以 root 运行且没有沙箱) 的应用, 页面上要怎么标注?
- `ai.storyteller.wordcraft` 授予 Flatpak talk 的理由需要复查。
