<p align="center">
  <img src="assets/logo.svg" alt="govm — Go 版本管理器" width="560">
</p>

<p align="center">安装、管理和切换你的 Go 版本。</p>

govm 是一个跨平台 Go 版本管理器，支持安装官方二进制包、从源码构建、并发安装多个版本，以及 Bash、Zsh、Fish 和 PowerShell 配置。下载的归档会进行 SHA-256 校验，默认数据目录为 `~/.govm`。

## 安装 govm

从 [GitHub Releases](https://github.com/Rehtt/govm/releases) 下载对应平台的归档：Linux、macOS（文件名使用 `darwin`）和 Windows 均提供 `amd64`、`arm64`。Linux/macOS 使用 `.tar.gz`，Windows 使用 `.zip`，例如 `govm_linux_amd64.tar.gz`。解压后将 `govm`（Windows 为 `govm.exe`）放到 `PATH` 中的目录。归档包含 LICENSE，发布附件中的 `checksums.txt` 提供 SHA-256 校验值。

从源码安装，需要 Go 1.26.4 或更高版本（见 `go.mod`）：

```sh
git clone https://github.com/Rehtt/govm.git
cd govm
go install .
```

将 Go 的可执行文件安装目录加入 `PATH`：设置了 `GOBIN` 时使用该目录，否则通常为 `$(go env GOPATH)/bin`。然后检查：

```sh
govm --help
govm version
```

普通本地构建输出 `govm dev`，不会自动读取 Git 信息。可在本地构建时注入版本：

```sh
go build -trimpath -ldflags "-X main.version=v1.0.0-a1b2c3d" -o govm .
./govm version  # govm v1.0.0-a1b2c3d
```

推送 tag 会在测试通过后自动构建六种平台归档并发布普通 GitHub Release，版本格式为 `{tag}-{短 hash}`。tag 保留原文（包括 `v` 前缀），hash 来自目标提交，至少七位；附注 tag 同样使用提交的 hash。同一 tag 的发布任务串行执行，重跑会覆盖该 Release 的同名附件。

## 快速开始

# 初始化
govm init

```sh
# 查看当前平台可安装的稳定版本
govm list-remote

# 安装最新稳定版
govm install latest

# 查看已安装版本及 current / default 标记
govm list
```

首次成功安装会自动选择当前版本、记录默认版本，并尝试配置检测到的 Shell。重新打开终端后验证：

```sh
go version
```

如果 Shell 未被识别，可手动运行 `govm init zsh`（按实际情况替换为 `bash`、`fish` 或 `powershell`），再重新打开终端。

## 安装与切换版本

以下具体版本号仅作为用法示例，可替换为 `govm list-remote --all` 中的版本。

```sh
# 安装指定版本，支持带或不带 go 前缀
govm install 1.26.4
govm install go1.26.4

# 并发安装多个版本，-j 指定同时安装的版本数
govm install -j 2 1.26.3 1.26.4

# 切换到已安装版本
govm use 1.26.4

# 切换并记录为默认版本
govm use --default 1.26.4

# 切换到本地已安装的最高版本
govm use latest
```

`install latest` 选择远端最新稳定版；`use latest` 选择本地已安装的最高版本，可能包含预发布版。

切换通过更新 `~/.govm/current` 符号链接生效，会影响所有使用同一 govm 数据目录且已配置 `PATH` 的终端。`--default` 记录默认版本，供安装时缺少有效当前版本的情况使用；打开新终端不会自动切回默认版本。

### 安装选项

| 选项 | 用途 |
| --- | --- |
| `-j N` / `--jobs N` | 并发安装数；二进制安装默认为 2，源码构建默认为 1 |
| `--build` | 下载官方源码并在本机编译 |
| `--force` | 覆盖已安装版本 |
| `--no-init` | 跳过 Shell 配置修改 |

实际下载归档时，`govm install` 会在交互式终端的错误输出上显示汇总进度条，包含文件数量、百分比、已下载大小和平均速度；并发安装会共享同一条进度条。源码安装自动下载 bootstrap 工具链时也会加入汇总。缓存命中、元数据请求以及重定向到文件或管道的输出不会显示动态进度。

速度以 MiB/s 显示，按本次下载会话的平均值计算；归档大小未知时隐藏百分比。下载结束、失败或取消后会清除进度行，解压和编译阶段不显示下载进度；CI 环境也不启用动态进度。

```sh
govm install --build 1.26.4
govm install --force 1.26.4
govm install --no-init latest
```

源码构建优先使用 `GOROOT_BOOTSTRAP` 指定的工具链，其次使用 `PATH` 中的 Go；若均不可用，会尝试下载 bootstrap 工具链。构建环境和 bootstrap 版本需满足目标 Go 版本的要求，日志保存在 `~/.govm/logs/build-go<版本号>.log`。

## 查看版本

`govm version` 查询 govm 自身版本，例如 `govm v1.0.0-a1b2c3d`，不接受额外位置参数。以下命令查询所管理的 Go 版本：

```sh
govm list                         # 本地版本
govm list --json                  # 本地版本的 JSON 信息
govm list-remote                  # 当前平台可用的稳定版本
govm list-remote --all            # 同时包含归档版本和预发布版
govm list-remote --refresh        # 强制刷新元数据
govm list-remote --all --json     # 以 JSON 输出远端版本
```

远端列表仅显示有当前操作系统和架构二进制包的版本。元数据默认缓存 6 小时。

## Shell 配置与补全

`init` 将 govm 的 `current/bin` 加入 Shell 配置中的 `PATH`；如果当前 `PATH` 没有 `~/go/bin`，也会一并加入。该命令可重复执行以修复配置。修改已有配置文件时，会在首次修改时保存 `.govm.bak` 备份。

```sh
govm init bash
govm init zsh
govm init fish
govm init powershell
```

只需执行与你使用的 Shell 对应的一条命令，然后重新打开终端。PowerShell 可通过 `GOVM_POWERSHELL_PROFILE` 明确指定配置文件，例如：

```powershell
$env:GOVM_POWERSHELL_PROFILE = $PROFILE
govm init powershell
. $PROFILE
```

`completion` 输出补全脚本，不会自动安装。以下命令在当前会话加载补全：

```bash
# Bash：需要先安装并加载 bash-completion
source <(govm completion bash)
```

```zsh
# Zsh
autoload -Uz compinit && compinit
source <(govm completion zsh)
```

```fish
# Fish
govm completion fish | source
```

```powershell
# PowerShell
govm completion powershell | Out-String | Invoke-Expression
```

需要长期启用时，将对应命令加入 Shell 配置文件。

## 卸载版本与清理缓存

```sh
govm uninstall 1.26.3       # 卸载非 current / default 的版本
govm cache clean            # 清理下载和元数据缓存
govm cache clean --all      # 同时清理自动下载的 bootstrap 工具链
```

卸载当前版本或默认版本需要显式使用 `--force`：

```sh
govm uninstall --force 1.26.4
```

这会清除相应的 current / default 状态，不会自动选择替代版本。需要时使用 `govm use <已安装版本>` 重新选择。

## 自定义配置

| 环境变量 | 用途 |
| --- | --- |
| `GOVM_HOME` | 数据目录，默认为 `~/.govm` |
| `GOVM_RELEASES_URL` | Go 发布元数据地址；兼容别名 `GOVM_METADATA_URL` |
| `GOVM_DOWNLOAD_BASE_URL` | 归档下载基础地址；兼容别名 `GOVM_DOWNLOAD_URL` |
| `GOROOT_BOOTSTRAP` | 源码构建使用的 bootstrap Go 根目录 |
| `GOVM_POWERSHELL_PROFILE` | PowerShell 配置文件路径 |

同时设置主变量与兼容别名时，主变量优先。自定义下载源需提供与发布元数据匹配的归档及校验值。

当前实现会以 `GOVM_HOME` 的父目录推导 Bash、Zsh 等 Shell 配置路径。使用自定义数据目录时，建议安装时加 `--no-init`，自行将 `<GOVM_HOME>/current/bin` 加入 `PATH`。

## 常见问题

- **切换后 `go version` 没有变化**：确认 `~/.govm/current/bin` 位于 `PATH` 前部，并重新打开终端。Bash 也可执行 `hash -r` 清除命令路径缓存；如果手动设置过 `GOROOT`，检查它是否仍指向旧版本。
- **Windows 无法创建符号链接**：启用 Windows 开发者模式，或在管理员终端中执行安装和切换命令。
- **需要查看命令参数**：运行 `govm --help` 或 `govm install --help` 等子命令帮助。

## 许可证

本项目采用 [MIT License](LICENSE)。
