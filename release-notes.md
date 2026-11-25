## v1.0.54

### Added

- **Cursor account on-demand spend limit management (Disabled / Fixed / Unlimited)**:
  - Added `GetCursorAccountSpendLimit` and `SetCursorAccountSpendLimit` backed by Cursor's `DashboardService/GetHardLimit` and `DashboardService/SetHardLimit` RPCs (including `personal_scope` support for unified X Subscription plans and `team_id` support for Teams/Enterprise seats).
  - Works with both OAuth (`session`) accounts and `crsr_`/`grok_` user API-key (`api_key_token`) accounts with automatic token refresh retry, Connect error decoding (`PAYMENT_METHOD_REQUIRED`, `PERMISSION_DENIED`, `FAILED_PRECONDITION`), and automatic per-model quota cooldown clearing when enabling or raising a limit.
  - Updated Cursor account cards to display live on-demand status (**Disabled**, **Fixed (`$used / $limit`)**, or **Unlimited**) and added an **On-demand spend limit** modal (**Edit limit** inline or via **More**) with mode switching, whole-dollar input, quick presets (`$20`–`$500`), and confirmation prompts before enabling/raising caps or selecting **Unlimited**.

## v1.0.53

### Added

- **Automatic data-retention policy acknowledgment for Claude Fable 5 / 5.1 / 5.2**:
  - When a Cursor account with Privacy Mode enabled rejects a Fable turn with a data-retention policy error (e.g. `"You must acknowledge Claude Fable 5's data retention policy to use the model."`), `cursor-byok` now automatically calls Cursor's `DashboardService/SetUserNoZdrModelConsent` API (`model_id: "claude-fable-5"`, `consent_version: "fable-data-retention-v1"`) and retries the turn seamlessly without failing the request.
  - Supports both OAuth (`session`) and `crsr_`/`grok_` API-key (`api_key_token`) accounts, falls back to `SetTeamMemberNoZdrModelConsent` for teams allowing per-member opt-in and to safe consent `DashboardAction` CTAs via `DashboardService/ClientAction`, preserves saved conversation checkpoints on consent refusals, and fails over cleanly to another saved account when Enterprise/Team admin approval is required.
- **Cross-platform local + WSL build guide (`BUILD_ALL_PLATFORMS.md`)**:
  - Documented the full local release workflow covering frontend builds, Windows (`x64`/`arm64`/`x32` ZIP + NSIS installers), Linux (`x64` `.tar.gz`), and macOS (`arm64`/`x64` `.tar.gz` + `.dmg`) via Debian WSL + `osxcross`, `update.json` manifest generation, and `gh release create`.

## v1.0.52

### Added

- **Support for Cursor's fine-grained `grok_` personal User API keys**:
  - `grok_` dashboard keys (`grok_<id>_<secret>`) are now recognized alongside legacy `crsr_` keys for import, `/auth/exchange_user_api_key` token exchange, automatic renewal, `/oauth/token` mock refresh, recovery packs, key rotation (`crsr_` → `grok_`), and UI secret redaction.
  - Added a local guard rejecting xAI Console `xai-...` keys during Cursor account import without sending them to Cursor's servers, directing users to the third-party **xAI (Grok)** supplier instead.

### Fixed

- **Imported Cursor JWTs no longer become invalid or lose their session after ~15 minutes**:
  - Explicit token/OAuth imports now clear or replace any stale `refreshToken` (such as an older `crsr_` key) instead of keeping it on the account record.
  - `/oauth/token` session refresh (`RefreshPresentedToken`) no longer overwrites unexpired 60-day JWTs when Cursor IDE's 53-day refresh margin triggers periodic refresh polls.
  - Speculative `ForceRefresh` no longer wipes the active account (`current.json`) on `shouldLogout` while the stored access token is still unexpired, and non-refreshable tokens (`type=web`, `type=api_key_token`, empty) are never sent to `/oauth/token`.
  - Plan refreshes now only invoke `ForceRefresh` on HTTP 401/403 or API-key accounts, re-importing clears stale cached health badges, and health checks explain when a pasted token is a `type=web` browser cookie rejected by model endpoints.

## v1.0.51

### Fixed

- **Cursor account file edits now show accurate line diffs (e.g. `-30` instead of `+20`) and no spurious `Read` cards**:
  - Upstream Cursor `EditToolCall` lifecycle updates (`PartialToolCall`, `ToolCallDelta`, `ToolCallStarted`, `ToolCallCompleted`) are now forwarded directly with authoritative `beforeFullFileContent`, `afterFullFileContent`, `diffString`, `linesAdded`, and `linesRemoved`.
  - Low-level internal `ReadArgs` / `WriteArgs` RPCs behind a Cursor-owned edit execute silently on the IDE without creating fake `Read attempted` / `Read` bubbles or synthetic `+N` write cards.
  - Standalone native writes fall back to computing diffs against complete file snapshots observed in the stream, and `EditSuccess` preserves literal leading/trailing whitespace and newlines.
- **Cursor account native web tools, spill writes, and wire correlation**:
  - Native `WebSearch` / `WebFetch` approval and upstream result completion cards are forwarded cleanly with timeout and cancel cleanup.
  - Internal `agent-tools/*.txt` spill writes run silently in all modes without emitting edit cards or polluting history.
  - Tightened exec/interaction wire correlation (including zero-ID execs, out-of-order replies, and `stream_close` on all terminal execs), non-streaming shell output delivery, and prompt/resume scaffolding filtering.
- **Provider streaming and replay stability**:
  - Anthropic streams complete cleanly on `message_stop` without waiting for socket close; OpenAI chat streams fail cleanly if cut off before tool completion; oversized `Grep` replay results compact to actual matches.

## v1.0.50

### Fixed

- **A finished shell no longer leaves the turn on "Planning next moves"**:
  - Cursor account shells now send the captured output, then close the upstream exec, so the model can read the result.
  - Output that the IDE spilled to a file is inlined. The model is not sent that path when the text is already included.
  - A shell that streams output but never exits is recovered and closed, instead of leaving the account stream open.

> **Windows 用户注意**：安装时若被 SmartScreen 拦截，点击「更多信息」->「仍要运行」即可。

### 下载哪个文件？（按系统选择）

> 名字里的 x64 / x32 / arm64 表示 CPU 架构，认准自己系统的类型下载即可。

- **Windows 64 位（绝大多数 Windows 电脑）**：下载 `cursor-byok-1.0.54-windows-x64-installer.exe`（安装版，推荐）或 `cursor-byok-1.0.54-windows-x64.zip`（绿色版）
- **Windows ARM64（骁龙/麒麟等 ARM 处理器的 Windows 电脑）**：下载 `cursor-byok-1.0.54-windows-arm64-installer.exe` 或 `cursor-byok-1.0.54-windows-arm64.zip`
- **Windows 32 位（很老的低配电脑才需要）**：下载 `cursor-byok-1.0.54-windows-x32-installer.exe` 或 `cursor-byok-1.0.54-windows-x32.zip`
- **macOS Apple Silicon（M1/M2/M3/M4 芯片）**：下载 `cursor-byok-1.0.54-macos-arm64.dmg` 或 `cursor-byok-1.0.54-macos-arm64.tar.gz`
- **macOS Intel**：下载 `cursor-byok-1.0.54-macos-x64.dmg` 或 `cursor-byok-1.0.54-macos-x64.tar.gz`
- **Linux 64 位**：下载 `cursor-byok-1.0.54-linux-x64.tar.gz`

Windows 系统类型可在「设置 → 系统 → 关于 → 系统类型」查看；macOS 在「关于本机」查看芯片或处理器。
