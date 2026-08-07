# Go CodeLens for Zed

[English](README.md) | **简体中文**

在 Zed 中以 CodeLens 形式精确展示 Go 引用与接口实现关系。

## 为什么开发这个项目

Go 接口采用隐式实现，这让语言保持了很高的灵活性，但也让一类重要关系在声明处不可见：接口不会列出实现它的类型，具体类型也不会声明自己满足了哪些接口。无论是日常阅读、跳转还是代码审查，能够直接看到这些关系都非常有价值。

`gopls` 仍然是 Go 最合适的主语言服务器，但它目前没有提供本项目所需要的这种关系型 CodeLens 视图。因此，Go CodeLens 作为一个专注的辅助语言服务器与 `gopls` 并行运行：它只补充引用与实现关系透镜，不代理、替代或干扰正常的 Go 开发能力。

本项目的灵感来自 [Anirudh Agarwal](https://github.com/anirudhagarwal365) 开发的 Visual Studio Code 扩展 [Go Implementation Lens](https://github.com/anirudhagarwal365/go-implementation-lens)。该扩展的使用方式和展示效果充分证明了这些信息的价值。本仓库是面向 Zed 的独立实现，以 Go 类型系统为核心，并进一步针对结果精确性、增量分析、低内存分配和最小化磁盘 I/O 进行了专项优化。

## 截图

<img src="assets/screenshot.png" alt="CodeLens">

## 功能

- 在接口、函数和方法上显示 `0 references`、`1 reference` 或 `N references`。
- 在已使用的命名类型、包级变量、常量、命名结构体字段和类型参数上显示引用数量；为了降低干扰，这些类别中未使用的声明不会显示透镜。
- 在命名接口和显式接口方法上显示 `N implementations`，并提供精确的导航目标。
- 在命名类型上显示 `Implements: Interface`，在接收者方法上显示 `Implements: Interface.Method`；一个声明满足多个关系时会合并展示。
- 完整支持内部和外部 `_test.go` 包变体。
- 通过最近且适用的 `go.work` 进行多模块分析；普通单模块项目不需要工作文件。
- 通过 LSP 增量文档同步立即分析尚未保存的修改。

完整接口关系由 `go/types.Implements` 判定，包含指针方法集和提升方法。方法关系先通过带包语义的 Go 方法标识缩小范围，再要求签名满足 `types.Identical`。引用统计同样基于语义：它关联 `go/types.Info.Defs` 与 `go/types.Info.Uses`，排除声明自身的位置，合并普通包和测试变体的重复源码位置，也不会因为名称相同而误计无关符号。

## 项目架构

```text
Zed Editor
├── gopls                         Go 语言主功能
└── Go CodeLens extension
    ├── src/lib.rs                轻量 Rust/Wasm Zed 适配层
    └── go-codelens-ls            原生、无 cgo 的 Go LSP Sidecar
        ├── internal/lsp          JSON-RPC、文档同步、缓存、取消
        └── internal/analysis     作用域、类型事实、索引、关系计算
            └── go/packages + go/ast + go/types
```

适配层会先在工作树 `PATH` 中查找 `go-codelens-ls`，方便本地开发覆盖；若未找到，则选择版本完全匹配的 GitHub Release 资源，通过 `SHA256SUMS` 校验后，把原生二进制缓存到 Zed 管理的扩展工作目录。Sidecar 随后负责确定适用的模块或 `go.work` 作用域，加载一致的 Go 类型宇宙，提取紧凑的语义事实，并返回标准 LSP CodeLens 命令。保持 Sidecar 独立后，补全、诊断、格式化、重命名等 Go 编辑体验仍完全由 `gopls` 负责。

Go 入口遵循惯用的 `server/cmd/go-codelens-ls` 布局；实现包保留在 `server/internal` 下，因为它们不是对外承诺的 Go 公共 API。

## 技术特性

- **精确语义：** 最终关系全部由 `go/types` 校验；基于名称的索引只用于缩小候选集，绝不代替正确性判断。
- **增量引擎：** 函数体修改只重检已知包所有者；API 修改重检反向依赖闭包；源码结构、模块元数据等不安全变化回退到一次完整 `packages.Load`。
- **不可变版本：** 包事实、关系索引和快照使用写时复制更新，并发读取者可以安全共享未变化的 map、slice 与导航目标。
- **高效文档编辑：** UTF-8 gap buffer 配合行首索引处理 UTF-16 LSP 增量编辑，无需每次重建全文。
- **有界并发与缓存：** 同一版本的请求共享一次分析；过时结果无法发布，无等待者的任务会被取消，作用域和快照缓存都有固定上限。
- **低磁盘活动：** 只在必要的文档生命周期或精确编译源码路径上读取文件。默认情况下插件不写日志、语义索引或持久缓存；只有显式启用 `GO_CODELENS_TRACE` 时才会写入协议方法跟踪，初次加载包时 Go 工具链也可能使用标准 Go 构建缓存。
- **确定性输出：** CodeLens 和导航目标会排序、去重，并使用正确的 LSP UTF-16 位置。
- **跨平台可信交付：** 无 cgo 的 Go Sidecar 支持 macOS、Linux、Windows 的 arm64 和 x86-64；发布资源固定到扩展版本并经过 SHA-256 校验，六个目标的原生测试与构建都是发布门槛。

## 性能展示

性能优化由单元、差分、集成、模糊、race、分配、取消和端到端基准共同约束。首个大型真实项目验收样本是在 Apple M1 Pro、Go 1.26.5 环境下，对未经修改的 Kubernetes 浅克隆进行的。

| Kubernetes 样本               |                                       数值 |
| ----------------------------- | -----------------------------------------: |
| Commit                        | `b882c60b4023bdf09264c2d5d30a2cadebc240fb` |
| 检出目录大小                  |                                    395 MiB |
| 受版本控制的非 vendor Go 文件 |                                     13,414 |
| `go.work` 中的模块            |                                         34 |
| 整个工作区的包                |                                      3,029 |
| 示例文档                      |                   `pkg/probe/exec/exec.go` |

| 测量项                             |       结果 |
| ---------------------------------- | ---------: |
| 首次完整工作区分析与 CodeLens 请求 |  4m42.692s |
| 同一进程中的语义快照缓存命中       | 249.958 µs |
| 返回的 CodeLens 数量               |         10 |
| 已取消请求的返回延迟               |   4.542 µs |
| 底层分析器的取消延迟               |   1.967 ms |
| 最大常驻内存                       |   3.69 GiB |
| macOS 峰值 memory footprint        |   6.66 GiB |
| 交换次数                           |          0 |

这是一组真实项目的单次验收观察，并非经过统计采样的基准。冷启动结果有意覆盖 `go.work` 中全部 34 个模块并启用测试变体，主要成本来自首次 `packages.Load` 和 Go 工具链构建包图。不可变语义快照发布后，重复请求可以在微秒级返回，不会再次进入分析器；Kubernetes 示例中预期的 10 个透镜全部得到保留。

运行 `./scripts/benchmark-server.sh` 可以执行项目维护的正确性、race、基准、分配和 profile 套件。profile 默认写入 `$TMPDIR` 下的目录，不进入仓库。

## 开发与构建

当前代码的环境要求：

- Zed 1.14.2 或更高版本
- Go 1.26 或更高版本
- 通过 `rustup` 安装 Rust，并包含 `wasm32-wasip2` 目标

在 macOS 或 Linux 上进行本地开发时，可构建当前主机的原生 Sidecar 并安装到 `~/.local/bin`：

```sh
./scripts/install-server.sh
```

Windows 使用对应的 PowerShell 安装脚本：

```powershell
pwsh -File .\scripts\install-server.ps1
```

`~/.local/bin` 只是开发安装脚本的默认值，并不是 Zed 固定的扩展安装位置。如果 Zed 继承的 `PATH` 使用其他目录，可以设置 `GO_CODELENS_BIN_DIR`。市场版无需运行这些脚本：没有 PATH 覆盖时，Zed 会把版本匹配的二进制下载到由自身管理、随平台而异的扩展工作目录。

构建脚本也接受显式目标，便于在一台机器上检查全部构建产物：

```sh
./scripts/build-server.sh linux amd64
./scripts/build-server.sh windows arm64
```

```powershell
pwsh -File .\scripts\build-server.ps1 -TargetOS windows -TargetArch arm64
```

支持的发布目标如下：

| 操作系统 | 架构   | 发布目标三元组               |
| -------- | ------ | ---------------------------- |
| macOS    | arm64  | `aarch64-apple-darwin`       |
| macOS    | x86-64 | `x86_64-apple-darwin`        |
| Linux    | arm64  | `aarch64-unknown-linux-gnu`  |
| Linux    | x86-64 | `x86_64-unknown-linux-gnu`   |
| Windows  | arm64  | `aarch64-pc-windows-msvc`    |
| Windows  | x86-64 | `x86_64-pc-windows-msvc`     |

安装本地 Sidecar 后，在 Zed 中运行 `zed: install dev extension` 并选择本仓库；后续更新可运行 `zed: rebuild dev extension`。仓库中的 `.zed/settings.json` 会让 `gopls` 与 `go-codelens` 同时保持活动。

完整验证命令：

```sh
cd server
env GOWORK=off TMPDIR=/tmp GOCACHE=/tmp/go-build-cache go test ./...
env GOWORK=off TMPDIR=/tmp GOCACHE=/tmp/go-build-cache go test -race ./...
env GOWORK=off TMPDIR=/tmp GOCACHE=/tmp/go-build-cache go vet ./...
golangci-lint run --config ../.golangci.yaml ./...
cd ..
cargo fmt -- --check
cargo test
cargo build --target wasm32-wasip2
```

发布前只需用一条命令更新版本：

```sh
./scripts/set-version.sh 0.3.3
```

脚本会先生成全部修改再写入工作树，同步 `extension.toml`、`Cargo.toml`、`Cargo.lock` 与原生服务器报告的版本，并验证锁定的 Cargo 元数据；验证失败时会恢复原文件。Release 工作流会以只读 `--check` 模式调用同一个脚本，使本地发布准备与 CI 校验始终使用同一套规则。

CI 会在六个目标各自的 GitHub 原生托管 Runner 上运行 Go 测试和无 cgo 构建，并额外在 Linux amd64 上运行 race detector，在独立任务中验证 Rust/Wasm 适配层。推送 `v<版本>` 标签后，只有标签、`Cargo.toml`、`extension.toml` 与原生服务器报告的版本一致时才会继续发布；工作流会生成六个原生裸二进制和确定性的 `SHA256SUMS`，扩展适配层只下载与自身版本完全一致的资源。

`examples/poc/main.go` 是小型功能样本。原生二进制、基准 profile 和 `extension.wasm` 都有意排除在版本控制之外。

## 当前 Zed 限制

- 在 Zed 1.14.2 中，即使 `lsp_results_location` 设置为 `"picker"`，包含多个目标的 CodeLens 兼容命令仍会打开 multibuffer；CodeLens 使用 picker 需要 Zed 上游提供能力。
- CodeLens 字体由 Zed 的编辑器设置控制；LSP 与扩展 API 都不支持单独设置透镜字体。
- Zed 扩展 API 没有暴露 VSCode 扩展使用的自定义 gutter 装饰或树形侧边栏；核心关系数据已尽可能通过可导航 CodeLens 呈现。
- 选定模块或 `go.work` 作用域之外的依赖会参与类型信息解析，但不会显示透镜；空接口有意报告零个有意义的实现。
- 市场版必须存在同版本的 `v<版本>` GitHub Release，并包含全部六个二进制和 `SHA256SUMS`；在该 Release 创建前，开发检出仍可继续使用 PATH 中安装的 Sidecar。

## 许可证

Go CodeLens for Zed 基于 [MIT License](LICENSE) 开源。
