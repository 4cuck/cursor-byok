## v1.0.36

### Fixed

- **Cursor account model sync failed with truncated WebView2 RPC payloads**: syncing Cursor models previously round-tripped the entire catalog and `UserConfig` JSON through WebView2's `POST /wails/runtime` stream, hitting body truncation (`Unable to parse request body as JSON: unexpected end of JSON input`) on larger configurations. Model sync now executes atomically inside Go (`SyncCursorAccountModels`), and `NormalizeModelAdapterConfigs` automatically compacts legacy bloated `CursorModelSpec` entries on load/save while preserving picker labels (`Effort`, `Fast`, `Context`, `Speed`), cost indicators, and hotkey cycling (`isCycleableByHotkey`).
- **Syncing a secondary Cursor account silently used the wrong account or deleted other accounts' models**: `cursorAccountAuthorizationForCatalog` no longer silently falls back to a different account's credentials when an explicit account is selected; instead it refreshes/re-exchanges that account's credentials and preserves sibling accounts' adapters cleanly.
- **Exhausted model quota pinned turns to a spent account**: `cursorAccountGateway` and the channel router now recognize both official Cursor 3.30 `ErrorDetails.Error` quota/rate-limit codes (`ERROR_PRO_USER_USAGE_LIMIT`, `ERROR_API_KEY_RATE_LIMIT`, `ERROR_FREE_USER_USAGE_LIMIT`, `ERROR_RESOURCE_EXHAUSTED`, etc.) and prose refusals, remember exhausted `(account, model)` pairs with automatic cooldown recovery, and walk your entire pool of saved unlocked Cursor accounts automatically—even when the current account itself is exhausted.
- **Cursor 3.30 `speed` vs `fast` serving-tier mismatch**: mirrored Cursor IDE 3.30.14's `serving-tier.ts` parameter healing so `fast` (`true`/`false`) and `speed` (`fast`/`default`) translate bidirectionally across model families without triggering `Invalid parameters for registry model`.
- **Diagnostics false-positive warnings & unresolvable channel crashes**: `ResolvedChannelsForDiagnostics` skips broken individual adapters instead of failing the entire snapshot, and Cursor account channels report `api2.cursor.sh` readiness plus live per-model quota cooldowns without false "missing API key" warnings.

### Improved

- **Richer Cursor account management & live bucket usage**: `CursorAccountCard` now auto-loads plan and usage snapshots in the background, displays `API Key` vs `OAuth` badges and live synced model counts (`N models · cursor_accountN`), breaks down **Named Models vs Auto** quota buckets and bonus credits from Cursor 3.30's `GetCurrentPeriodUsage`, surfaces active per-model quota cooldowns with 1-click reset, and adds **Sync all**, **Sync & use exclusively**, **Remove synced models**, and automatic adapter reassignment when deleting an account.
- **Unified clientidentity & error redaction**: all Cursor control-plane and agent-plane requests send complete Cursor IDE 3.30 client headers (`x-cursor-client-version`, `x-cursor-client-commit`, `x-cursor-client-os`, `x-cursor-client-arch`, `x-cursor-checksum`) with automatic direct-transport retry when a configured proxy is unreachable, and UI error banners surface sanitized backend causes (`crsr_[redacted]`).

> **Windows 用户注意**：安装时若被 SmartScreen 拦截，点击「更多信息」->「仍要运行」即可。

### 下载哪个文件？（按系统选择）

> 名字里的 x64 / x32 / arm64 表示 CPU 架构，认准自己系统的类型下载即可。

- **Windows 64 位（绝大多数 Windows 电脑）**：下载 `cursor-byok-1.0.36-windows-x64-installer.exe`（安装版，推荐）或 `cursor-byok-1.0.36-windows-x64.zip`（绿色版）
- **Windows ARM64（骁龙/麒麟等 ARM 处理器的 Windows 电脑）**：下载 `cursor-byok-1.0.36-windows-arm64-installer.exe` 或 `cursor-byok-1.0.36-windows-arm64.zip`
- **Windows 32 位（很老的低配电脑才需要）**：下载 `cursor-byok-1.0.36-windows-x32-installer.exe` 或 `cursor-byok-1.0.36-windows-x32.zip`
- **macOS Apple Silicon（M1/M2/M3/M4 芯片）**：下载 `cursor-byok-1.0.36-macos-arm64.dmg` 或 `cursor-byok-1.0.36-macos-arm64.tar.gz`
- **macOS Intel**：下载 `cursor-byok-1.0.36-macos-x64.dmg` 或 `cursor-byok-1.0.36-macos-x64.tar.gz`
- **Linux 64 位**：下载 `cursor-byok-1.0.36-linux-x64.tar.gz`

Windows 系统类型可在「设置 → 系统 → 关于 → 系统类型」查看；macOS 在「关于本机」查看芯片或处理器。
