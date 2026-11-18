## v1.0.37

### Fixed

- **Account health check falsely reported "requires a paid plan" (`ERROR_RATE_LIMITED_CHANGEABLE`) when probing rate-limited Claude models**: `CheckCursorAccountHealth` now probes Cursor's lightweight `Auto` (`default`), `Grok`, and `Composer` models first instead of heavy Anthropic/Claude adapters, and classifies `ERROR_RATE_LIMITED` / `ERROR_RATE_LIMITED_CHANGEABLE` as rate/quota exhaustion rather than a missing paid plan.
- **Usage buckets displayed inverted `% left` instead of Cursor 3.30's `% used` and conflated pooled/on-demand caps**: `CursorAccountCard` and `CursorAccountPlan` now display **`% used`** identically to Cursor IDE 3.30 (`Named models: X% used` first, `Auto / Grok: Y% used` second), preserve `0% used` buckets instead of hiding them, prioritize `totalPercentUsed` when bonus usage exceeds the base `$20` pool, and report `On-demand limit` separately from included plan quota.
- **Cursor account model sync failed with truncated WebView2 RPC payloads**: syncing Cursor models executes atomically inside Go (`SyncCursorAccountModels`), and `NormalizeModelAdapterConfigs` compacts legacy bloated `CursorModelSpec` entries while preserving picker labels, cost hints, and hotkey cycling.

### Improved

- **Automatic multi-account pool quota failover**: when an account hits a model usage limit, `cursorAccountGateway` and the channel router automatically fail over across all saved unlocked Cursor accounts while keeping Auto/Grok/Composer available on the spent account.

> **Windows 用户注意**：安装时若被 SmartScreen 拦截，点击「更多信息」->「仍要运行」即可。

### 下载哪个文件？（按系统选择）

> 名字里的 x64 / x32 / arm64 表示 CPU 架构，认准自己系统的类型下载即可。

- **Windows 64 位（绝大多数 Windows 电脑）**：下载 `cursor-byok-1.0.37-windows-x64-installer.exe`（安装版，推荐）或 `cursor-byok-1.0.37-windows-x64.zip`（绿色版）
- **Windows ARM64（骁龙/麒麟等 ARM 处理器的 Windows 电脑）**：下载 `cursor-byok-1.0.37-windows-arm64-installer.exe` 或 `cursor-byok-1.0.37-windows-arm64.zip`
- **Windows 32 位（很老的低配电脑才需要）**：下载 `cursor-byok-1.0.37-windows-x32-installer.exe` 或 `cursor-byok-1.0.37-windows-x32.zip`
- **macOS Apple Silicon（M1/M2/M3/M4 芯片）**：下载 `cursor-byok-1.0.37-macos-arm64.dmg` 或 `cursor-byok-1.0.37-macos-arm64.tar.gz`
- **macOS Intel**：下载 `cursor-byok-1.0.37-macos-x64.dmg` 或 `cursor-byok-1.0.37-macos-x64.tar.gz`
- **Linux 64 位**：下载 `cursor-byok-1.0.37-linux-x64.tar.gz`

Windows 系统类型可在「设置 → 系统 → 关于 → 系统类型」查看；macOS 在「关于本机」查看芯片或处理器。
