# Go CodeLens for Zed

[English](README.md) | **简体中文**

一款以 CodeLens 形式精确显示 Go 引用和接口实现关系的 Zed 扩展。它在 `gopls` 旁运行一个小型原生 Go 语言服务器；`gopls` 仍然是 Zed 的主要 Go 语言服务器。

本项目受 [Visual Studio Code 的 Go Implementation Lens](https://github.com/anirudhagarwal365/go-implementation-lens) 启发，为 Zed 独立重新实现，并针对高性能、低内存开销和最小化磁盘 I/O 进行了优化。

## 功能

对于当前 Go 分析作用域，本扩展会显示：

- 在接口、函数和方法上显示 `0 references`、`1 reference` 或 `N references`。
- 在已使用的命名类型、包级变量、常量、命名结构体字段和类型参数上显示引用数量。为了减少干扰，这些类别中未使用的声明不会显示 CodeLens。
- 在命名接口上显示 `N implementations`，并链接到实现该接口的命名类型。
- 在命名类型上显示 `Implements: Interface`，并链接回对应接口。
- 在显式声明的接口方法上显示 `N implementations`，并链接到匹配的接收者方法。
- 在接收者方法上显示 `Implements: Interface.Method`，也会包含对部分接口实现有意义的匹配。

完整的接口实现关系通过 `go/types.Implements` 判断，并包含指针方法集，因此绝不会仅凭方法名就将某个类型报告为完整实现。方法关系要求 Go 方法标识相同，并且签名满足 `types.Identical`；名称正确但参数或返回值错误的方法会被排除。

引用分析同样基于语义，而不是文本匹配。分析器在作用域内所有已加载的包及其测试变体中，将 `go/types.Info.Defs` 的声明对象与 `go/types.Info.Uses` 的使用位置关联起来。声明位置不会计入引用，普通包和测试包变体中的重复结果会按源码位置合并，同名但无关的符号不会被错误计数。引用跳转目标始终指向准确的使用位置。

如果文档所属模块被上层目录中的 `go.work` 引用，分析作用域将包含该工作区中的全部模块；否则，作用域就是文档所属模块。内部和外部 `_test.go` 包都会参与引用计数、实现关系分析、CodeLens 显示和导航。

服务器会处理 Zed 发送的全文文档通知，因此未保存的修改会立即使受影响的语义快照失效并触发重新计算。CodeLens 标题和导航目标具有确定性，源码位置使用 LSP UTF-16 偏移量。

## 架构

本扩展由两部分组成：

- `src/lib.rs` 是 Rust/Wasm 编写的 Zed 适配层，负责注册辅助语言服务器，并在工作树的 `PATH` 中定位其原生可执行文件。
- `server/` 是 Go 编写的 LSP Sidecar，使用 `golang.org/x/tools/go/packages`、`golang.org/x/mod/modfile`、`go/ast` 和 `go/types` 分析适用的模块或 `go.work` 工作区。

该 Sidecar 有意不代理或替代 `gopls`。概念验证已经确认 Zed 可以同时为 Go 运行这两个语言服务器；保持二者独立可以避免转发 `gopls` 的全部能力，并让本扩展专注于实现关系 CodeLens。

## 安装开发版扩展

环境要求：

- Zed 1.14.2 或更高版本
- 当前代码需要 Go 1.26 或更高版本
- 通过 `rustup` 安装 Rust；Zed 扩展使用 `wasm32-wasip2` 目标
- 当前原生开发构建脚本仅支持 Apple Silicon macOS

构建原生 Sidecar 并将其安装到 `~/.local/bin`：

```sh
./scripts/install-server.sh
```

如果 Zed 继承的 `PATH` 使用其他目录，请设置 `GO_CODELENS_BIN_DIR`。

在 Zed 中运行 `zed: install dev extension` 并选择本仓库。如果已经安装，请运行 `zed: rebuild dev extension`。随后打开仓库中的 `examples/poc/main.go`。如果 Zed 停止旧的 Sidecar 后没有重新启动它，请重新打开或重新加载工作区。

仓库中的 `.zed/settings.json` 会启用行内 CodeLens，并让两个 Go 语言服务器同时保持活动：

```json
{
  "code_lens": "on",
  "languages": {
    "Go": {
      "language_servers": ["gopls", "go-codelens", "..."]
    }
  }
}
```

`"..."` 会保留 Zed 中注册的其他 Go 语言服务器。

示例中应该显示：

- 接口、接口方法、函数、已使用类型和字段上的引用 CodeLens。
- `Greeter` 和 `Greeter.Greet` 上的 `2 implementations`。
- `Person` 和 `Robot` 上的 `Implements: Greeter`。
- 两个正确 `Greet` 方法上的 `Implements: Greeter.Greet`。
- `BrokenGreeter` 及其不兼容的 `Greet(int)` 方法上不显示实现关系 CodeLens。

激活 CodeLens 会调用 Zed 标准的 `editor.action.showReferences` 兼容命令。只有一个目标时会直接跳转。

在 Zed 1.14.2 中，即使将 `lsp_results_location` 设置为 `"picker"`，点击包含多个目标的 CodeLens 仍然会打开 Zed 的多缓冲区结果。Zed 会将三个 CodeLens 兼容命令（`editor.action.showReferences`、`editor.action.goToLocations` 和 `editor.action.peekLocations`）映射到同一条仅支持多缓冲区的处理路径；picker 设置目前只对编辑器原生操作生效。当前如需打开 picker，请将光标放在声明处，然后从命令面板运行 `editor: go to implementation` 或 `editor: find all references`。

## 验证构建

```sh
cd server
env GOWORK=off TMPDIR=/tmp GOCACHE=/tmp/go-build-cache go test -race ./...
cd ..
cargo fmt -- --check
cargo build --target wasm32-wasip2
./scripts/install-server.sh
```

## 当前限制

- 分析作用域是包含文档所属模块且距离最近的上层 `go.work`；如果不存在适用的工作文件，则作用域为该模块。作用域之外的导入依赖仍会提供类型信息，但不会获得 CodeLens 条目。
- 空接口会报告零个实现，而不会将所有命名类型都视为有意义的目标。
- 方法 CodeLens 覆盖显式声明的接口方法和接收者方法。完整的接口与类型关系仍会通过 `go/types` 考虑嵌入和提升方法。
- Zed 1.14.2 尚未提供支持 picker 的 CodeLens 命令或扩展操作 API。要让包含多个目标的 CodeLens 点击操作遵循 `lsp_results_location`，需要修改 Zed 上游。
- CodeLens 的字体样式由 Zed 控制：它使用编辑器缓冲区字体，字号为所配置缓冲区字号的 90%。LSP CodeLens 和 Zed 扩展 API 都没有提供单独设置 CodeLens 字体样式的能力；修改 `buffer_font_family` 或 `buffer_font_size` 也会同时改变普通编辑器文本。
- Zed 当前的扩展 API 没有暴露 VSCode 插件使用的自定义 gutter 装饰或树形侧边栏。相关关系数据会尽可能通过可导航的 CodeLens 条目呈现。
- 当前开发构建脚本仅支持 Apple Silicon macOS。发布还需要提供各平台下载、校验和及更新机制。
- 原生二进制文件和 `extension.wasm` 有意不提交到仓库。

## 许可证

本项目采用 [MIT License](LICENSE) 授权。
