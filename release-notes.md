## v1.0.33

### New

- **Plan and remaining usage on each Cursor account**: every saved key shows its plan name, subscription status, and how much included usage is left. The bar turns amber, then red, as the allowance runs down.
- **Cursor account models are separated by owner**: synced models are labeled `cursor_account1`, `cursor_account2`, and so on, so two accounts no longer share one catalog or one spend card.
- **Account health you can actually read**: Check status probes the real agent path and reports Working, Locked, Rejected, Session expired, Plan required, or Error on that account only.

### Fixed

- **Paid accounts were reported as "Plan required"**: the health check called a retired chat route that answers every account with a payment-shaped error. It now uses the same agent path as the installed Cursor client.
- **One prompt printed the reply twice and said the context was summarized**: a normal turn no longer asks the model to write the answer again after `send_final_summary`, and it no longer sends the compaction events Cursor renders as "Chat context summarized".
- **A valid API key stopped working after about an hour**: the short-lived session was stored as the refresh token. Refresh now keeps the original `crsr_` key and mints a new session when Cursor asks.
- **Switching to official upstream and back replaced the API key**: injected sessions are no longer saved as the official login, and returning to cursor-byok writes the selected account back.
- **Check status looked like every account was being checked**: only the account you clicked is marked as in progress, and its result stays on that row.

> **Windows 用户注意**：安装时若被 SmartScreen 拦截，点击「更多信息」->「仍要运行」即可。

### 下载哪个文件？（按系统选择）

> 名字里的 x64 / x32 / arm64 表示 CPU 架构，认准自己系统的类型下载即可。

- **Windows 64 位（绝大多数 Windows 电脑）**：下载 `cursor-byok-1.0.33-windows-x64-installer.exe`（安装版，推荐）或 `cursor-byok-1.0.33-windows-x64.zip`（绿色版）
- **Windows ARM64（骁龙/麒麟等 ARM 处理器的 Windows 电脑）**：下载 `cursor-byok-1.0.33-windows-arm64-installer.exe` 或 `cursor-byok-1.0.33-windows-arm64.zip`
- **Windows 32 位（很老的低配电脑才需要）**：下载 `cursor-byok-1.0.33-windows-x32-installer.exe` 或 `cursor-byok-1.0.33-windows-x32.zip`
- **macOS Apple Silicon（M1/M2/M3/M4 芯片）**：下载 `cursor-byok-1.0.33-macos-arm64.dmg` 或 `cursor-byok-1.0.33-macos-arm64.tar.gz`
- **macOS Intel**：下载 `cursor-byok-1.0.33-macos-x64.dmg` 或 `cursor-byok-1.0.33-macos-x64.tar.gz`
- **Linux 64 位**：下载 `cursor-byok-1.0.33-linux-x64.tar.gz`

Windows 系统类型可在「设置 → 系统 → 关于 → 系统类型」查看；macOS 在「关于本机」查看芯片或处理器。
