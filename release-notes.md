## v1.0.49

### Fixed

- **Background builds no longer show up as skipped and then freeze the turn**:
  - A shell started in the background was completed with no shell result, so Cursor drew the card as skipped.
  - The model then read the terminal, and that Read never closed the upstream exec, so the turn sat silent.
  - A background spawn is now a running shell card, and a finished Read, Grep, or background spawn is closed the same way a shell exit already was.

> **Windows 用户注意**：安装时若被 SmartScreen 拦截，点击「更多信息」->「仍要运行」即可。

### 下载哪个文件？（按系统选择）

> 名字里的 x64 / x32 / arm64 表示 CPU 架构，认准自己系统的类型下载即可。

- **Windows 64 位（绝大多数 Windows 电脑）**：下载 `cursor-byok-1.0.49-windows-x64-installer.exe`（安装版，推荐）或 `cursor-byok-1.0.49-windows-x64.zip`（绿色版）
- **Windows ARM64（骁龙/麒麟等 ARM 处理器的 Windows 电脑）**：下载 `cursor-byok-1.0.49-windows-arm64-installer.exe` 或 `cursor-byok-1.0.49-windows-arm64.zip`
- **Windows 32 位（很老的低配电脑才需要）**：下载 `cursor-byok-1.0.49-windows-x32-installer.exe` 或 `cursor-byok-1.0.49-windows-x32.zip`
- **macOS Apple Silicon（M1/M2/M3/M4 芯片）**：下载 `cursor-byok-1.0.49-macos-arm64.dmg` 或 `cursor-byok-1.0.49-macos-arm64.tar.gz`
- **macOS Intel**：下载 `cursor-byok-1.0.49-macos-x64.dmg` 或 `cursor-byok-1.0.49-macos-x64.tar.gz`
- **Linux 64 位**：下载 `cursor-byok-1.0.49-linux-x64.tar.gz`

Windows 系统类型可在「设置 → 系统 → 关于 → 系统类型」查看；macOS 在「关于本机」查看芯片或处理器。
