## v1.0.41

### Changed

- **Aligned auto-summarization and forced-compaction thresholds with Cursor IDE `3.30.14` defaults (`V5e`)**:
  - Replaced the upstream `0.8 * window - 10,000` (75%–79%) threshold with Cursor's native start-summarization budget `min(90% × window, window − 10,000)` (`autoCompactionBudgetTokens`) across auto-compaction (`compaction.go`), context projection (`context_projection.go`), delegated workers (`delegation_compaction.go`), active-turn projection (`service_provider.go`), and token-usage tracking (`token_usage.go`).
  - Updated Cursor Account upstream checkpoint preflight forced compaction (`service_provider.go`) to Cursor's native blocking/persist threshold `min(95% × window, window − 5,000)` (`cursorBlockingCompactionBudgetTokens`), leaving the 90%–95% range for Cursor's cloud agent server to run native server-side summarization first while still protecting downward model switches (e.g., 1M → 256K) and uncompacted checkpoints at 95%.
  - Effective thresholds by context window:
    - `1M`: `900K` auto / `950K` Cursor blocking (previously `790K`)
    - `256K`: `230.4K` auto / `243.2K` Cursor blocking (previously `194.8K`)
    - `200K`: `180K` auto / `190K` Cursor blocking (previously `150K`)
    - `64K`: `54K` auto / `59K` Cursor blocking (previously `41.2K`)

> **Windows 用户注意**：安装时若被 SmartScreen 拦截，点击「更多信息」->「仍要运行」即可。

### 下载哪个文件？（按系统选择）

> 名字里的 x64 / x32 / arm64 表示 CPU 架构，认准自己系统的类型下载即可。

- **Windows 64 位（绝大多数 Windows 电脑）**：下载 `cursor-byok-1.0.41-windows-x64-installer.exe`（安装版，推荐）或 `cursor-byok-1.0.41-windows-x64.zip`（绿色版）
- **Windows ARM64（骁龙/麒麟等 ARM 处理器的 Windows 电脑）**：下载 `cursor-byok-1.0.41-windows-arm64-installer.exe` 或 `cursor-byok-1.0.41-windows-arm64.zip`
- **Windows 32 位（很老的低配电脑才需要）**：下载 `cursor-byok-1.0.41-windows-x32-installer.exe` 或 `cursor-byok-1.0.41-windows-x32.zip`
- **macOS Apple Silicon（M1/M2/M3/M4 芯片）**：下载 `cursor-byok-1.0.41-macos-arm64.dmg` 或 `cursor-byok-1.0.41-macos-arm64.tar.gz`
- **macOS Intel**：下载 `cursor-byok-1.0.41-macos-x64.dmg` 或 `cursor-byok-1.0.41-macos-x64.tar.gz`
- **Linux 64 位**：下载 `cursor-byok-1.0.41-linux-x64.tar.gz`

Windows 系统类型可在「设置 → 系统 → 关于 → 系统类型」查看；macOS 在「关于本机」查看芯片或处理器。
