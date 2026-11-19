## v1.0.43

### Changed

- **Cursor accounts share one model list**:
  - Syncing no longer copies the full catalog onto every account. Cursor models stay in a single `cursor_account` list.
  - Set as current chooses which saved account serves that list. Other saved accounts are used only when the current account runs out of quota for a model.
  - Removed the Sync all button, which had been creating a separate copy of every model for each account.

### Fixed

- **Deleting synced Cursor models no longer fails with a truncated JSON body**:
  - Removing one Cursor model, a Cursor supplier, or all synced models now happens inside the app and only sends the model ids.
  - This fixes `Unable to parse request body as JSON: unexpected end of JSON input` after a large sync.

> **Windows 用户注意**：安装时若被 SmartScreen 拦截，点击「更多信息」->「仍要运行」即可。

### 下载哪个文件？（按系统选择）

> 名字里的 x64 / x32 / arm64 表示 CPU 架构，认准自己系统的类型下载即可。

- **Windows 64 位（绝大多数 Windows 电脑）**：下载 `cursor-byok-1.0.43-windows-x64-installer.exe`（安装版，推荐）或 `cursor-byok-1.0.43-windows-x64.zip`（绿色版）
- **Windows ARM64（骁龙/麒麟等 ARM 处理器的 Windows 电脑）**：下载 `cursor-byok-1.0.43-windows-arm64-installer.exe` 或 `cursor-byok-1.0.43-windows-arm64.zip`
- **Windows 32 位（很老的低配电脑才需要）**：下载 `cursor-byok-1.0.43-windows-x32-installer.exe` 或 `cursor-byok-1.0.43-windows-x32.zip`
- **macOS Apple Silicon（M1/M2/M3/M4 芯片）**：下载 `cursor-byok-1.0.43-macos-arm64.dmg` 或 `cursor-byok-1.0.43-macos-arm64.tar.gz`
- **macOS Intel**：下载 `cursor-byok-1.0.43-macos-x64.dmg` 或 `cursor-byok-1.0.43-macos-x64.tar.gz`
- **Linux 64 位**：下载 `cursor-byok-1.0.43-linux-x64.tar.gz`

Windows 系统类型可在「设置 → 系统 → 关于 → 系统类型」查看；macOS 在「关于本机」查看芯片或处理器。
