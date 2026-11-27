## v1.0.63

### Fixed

- **Newer Claude models no longer fail with "Claude Code 2.1.179 does not support this model"**:
  - Every Anthropic request starts its system prompt with the Claude CLI's billing header block, and Anthropic reads the client version from that block. It still named version 2.1.179 while the user agent said 2.1.329, so models that need a newer Claude Code were refused: Claude Opus 5.5 asks for 2.1.280, Claude Fable 5.2 for 2.1.290.
  - The block now names the same version as the user agent, 2.1.329. Both are built from one constant, so they cannot drift apart again.
  - On Anthropic's own API the three digits after the version are now computed from the first user message, the way the CLI computes them. The rule was read from the CLI's code and gives the value the CLI itself sent for a known prompt. For every other provider the block stays the same in all conversations, so prompt caches there are not affected.
  - Checked against Anthropic with a Pro subscription: Claude Opus 5.5 answers. Claude Fable 5.2 gets past the version check and answers `429 Usage credits are required for this model`; that is the plan's own limit, which the app cannot change.
- **The Claude subscription section no longer says that requests are refused when a usage limit shows 100%.** Anthropic was seen to accept requests while the current session stood at 100%. The account card now reports the figure and its reset time, and says only that requests may be refused.

## v1.0.62

### Added

- **Claude subscription accounts have their own section: Control center → Claude subscription**:
  - An account is added by pasting the Claude CLI's credentials (`~/.claude/.credentials.json`; on macOS they are in the Keychain) or a token, or by choosing the credentials file in a file dialog. The app never looks for a credentials file on its own: credentials are read only when you submit them. The token is checked with Anthropic first and then saved on this computer.
  - Each account shows its e-mail address, name, organization and plan, the state and expiry of its token, the usage limits Anthropic reports (current session, current week, weekly limits of single model families) with their reset times, usage credits, rate limit tier and token scopes. Usage older than five minutes is read again when the section is opened, and **Refresh** reads it at any time.
  - **Several accounts** can be added, and all of them serve the same models. New conversations take the accounts in turn, a conversation stays on the account it started with, and when an account is rate limited or refused the next one takes over.
  - **Add models** lists the models the accounts can see and sets up the chosen ones for subscription OAuth. Each model has a Fast switch, **Edit** (the model editor) and **Remove**. When a model has Fast on and none of the usable accounts has usage credits, the section says that Fast mode will not apply.
  - **Replace credentials** gives a saved account a new token in every model it serves. Credentials of an account that is already saved replace its token; they do not add the account a second time.
  - Removing an account takes its token out of every model. A model other accounts serve too stays; a model only that account served is removed with it.
  - A model set up by hand in v1.0.61 with a subscription token is listed in the section once the account of that token is added.
  - A long-lived token from `claude setup-token` is accepted as well. It is for sending requests only, so its account shows no profile and no usage.
  - Tokens are still not renewed. An account says when its token is about to expire, and an expired or refused token is marked on the account and on the tab.
  - Checked against one real Pro subscription: reading the account and its usage, listing the models, adding and removing models and the account. Taking turns between several accounts is covered by automated tests against a stand-in for Anthropic's API, and a `claude setup-token` token has not been tried.

### Changed

- **The model editor no longer has the Import Claude subscription credentials box.** An Anthropic model that can use a subscription token shows a pointer to the new section instead. The **Claude subscription OAuth** authentication mode and the Fast mode switch are unchanged.

## v1.0.61

### Added

- **Claude subscription tokens (Pro / Max) work for Anthropic models**:
  - An Anthropic model can be run on the access token the Claude CLI keeps in `~/.claude/credentials.json` (`sk-ant-oat…`), against `https://api.anthropic.com`. Usage counts against the subscription.
  - The new authentication mode **Claude subscription OAuth** (model editor, **Protocol and Advanced Settings**) sends the token as a bearer token with the Claude CLI's OAuth headers (`anthropic-beta: oauth-2025-04-20,claude-code-20250219`, `x-app: cli`) and adds the Claude Code identity system block after the billing header, in the order the CLI sends them.
  - **Auto** recognizes a subscription token on the official API and sends it the same way, so the token also works with the default settings. Until now Auto put it in `x-api-key`, which Anthropic answers with `401 invalid x-api-key`; only **Bearer only** and the legacy dual-header mode got through.
  - **Import Claude subscription credentials** in the model editor takes the pasted contents of `credentials.json`, or the token alone, and fills the access token into the API key. A token that has already expired is refused, with its expiry date.
  - **Fetch models** lists the subscription's models. Which of them the plan may call is Anthropic's decision: a model outside the plan answers `429 Usage credits are required for this model`.
  - The token is stored as the model's API key and is not refreshed. When it expires, import the credentials again.
  - Whether a subscription token may be used outside Anthropic's own clients is set by Anthropic's terms. Check them before you use one here.
- **Fast mode for Claude models**:
  - The **Fast mode** switch in the model editor is now offered for Anthropic models. With it on, requests carry `"speed": "fast"` and the `fast-mode-2026-02-01` beta feature, as the Claude CLI sends them.
  - Anthropic accepts Fast mode for some models only and, on a subscription, only with usage credits. When it refuses Fast mode (a `400` that names Fast mode or the `speed` parameter, a `429`, or a `529`), the same request is sent again at standard speed, and Fast mode stays off for that key and model for 30 minutes, or for the provider's `Retry-After` when that is longer than 10 minutes. The Claude CLI falls back on the same refusals. Any other error is returned as it is.
  - Tested against a subscription without usage credits: both of Anthropic's refusals (`Usage credits are required for fast mode`, `does not support the speed parameter`) fall back and the request succeeds. A request that Anthropic accepts in Fast mode has not been observed yet.

### Changed

- The Claude Code user agent sent to model providers is now `claude-cli/2.1.329`.

## v1.0.60

### Added

- **API keys on a Cursor account card**:
  - Each Cursor account card has **More → API keys**. The dialog lists the account's user API keys with their name, masked key and creation date.
  - **Create key** makes a new key and shows it once, with a copy button. A key is created only when you press the button; nothing creates one on its own.
  - The name is one you type or a random one. A random name follows the shape of a key the account already has, or Cursor's own naming when it has none. It contains nothing taken from this machine (no host name, user name or process id), so the key names of two accounts share nothing that identifies one machine or person.

### Fixed

- **A finished background subagent no longer shows "Stopped", and the agent is told when background work ends**:
  - Cursor decides with its `long_running_jobs` switch whether background work may end after the turn that started it. Cursor ships that switch on, but it was missing from the list cursor-byok serves, and the IDE reads a missing switch as off. Off, the IDE left the Task card of a finished background subagent without its result, so it showed "Stopped", and never started the run that tells the agent a background subagent or command has finished. v1.0.59 made cursor-byok answer that run; the IDE never sent it.
  - The switch is now served as on. After updating, restart Cursor once. The chat then starts a short turn on its own when a background subagent or command finishes, and it shows which background tasks are still running, as Cursor does without cursor-byok. A card that already shows "Stopped" stays as it is.
  - With that run now arriving, two gaps in how it was handled are closed. Tasks that finish together are each shown in the IDE; for models outside a Cursor account only the first was. And the run works in the chat's own workspace and terminals folder, which it does not name itself, so a wait on a command, relative paths and new subagents work in it as in any other turn.

## v1.0.59

### Fixed

- **Waiting on a shell no longer ends the turn (Cursor accounts)**:
  - `AwaitShell` runs on Cursor's server, which watches the command by reading its terminal file through the IDE several times a second. Each of those reads was shown as a `Read` call of the model, and the repeated-call guard stopped the turn at the fifth identical one, a few seconds into any wait on a command that was still running.
  - The reads are now answered silently and the wait is shown as one card with Cursor's own result.
- **A background subagent no longer shows "Completed" the moment it starts**:
  - The IDE answers the launch of a background subagent at once with "moved to background". That answer was recorded as the subagent finishing. The run is now reported as backgrounded, and the card stays running until the subagent ends.
  - The same answer for a subagent that a new message had sent to the background is no longer recorded as its result.
- **The agent is told when a background task finishes**:
  - When a background subagent or shell ends, the IDE starts a run of the parent chat to say so. It went out as a plain resume, so the model was started and told nothing.
  - On a Cursor account the IDE's message is now relayed to Cursor, which writes the notice the model reads. For other providers cursor-byok writes the same notice.
- **A dropped connection to Cursor no longer fails the turn**:
  - Cursor's server closes the connection from time to time (`http2: server sent GOAWAY`), which ended the turn with `upstream stream interrupted mid-response`. The turn now resumes from the last checkpoint Cursor sent, as the Cursor client does, up to 10 times per turn.
  - A tool that was running when the connection dropped does not run a second time: when Cursor asks for it again, it is answered with the result already at hand.
  - A question or approval card that was open at that moment is not carried over, and text written before the drop stays on screen with the answer written again after it.

## v1.0.58

### Changed

- **A Cursor account presents one consistent random device everywhere**:
  - The device identity sent to Cursor now includes a matching `macMachineId` next to the machine id, so `x-cursor-checksum` has the same shape the real Cursor client sends.
  - When an account is injected into the real Cursor app, its `serviceMachineId` and the whole `telemetry` block (`machineId`, `macMachineId`, `devDeviceId`, `sqmId`) are set to that account's own random, persisted values. The account looks like the same single device whether it is used through cursor-byok or the real Cursor window, instead of exposing the host machine.
  - The machine's original identifiers are snapshotted once before the first change and written back when you stop the service or disconnect the account, so your own Cursor install returns to its real identity.

## v1.0.57

### Changed

- **Each Cursor account presents its own device id**:
  - The machine id sent to Cursor in `x-cursor-checksum` is no longer read from the host's Cursor install (`serviceMachineId`). Every saved account is assigned its own randomly generated id, created once and then kept, so accounts are no longer tied to the host device or linkable to one another.
  - Requests not bound to a saved account (the local relay, sign-in before an account exists) use a single random id stored once per install, in `data/machine-id`.
  - An existing account moves to its new random id once, the first time it is used after updating, and keeps it from then on. Each account still looks like the same single device, so this does not trip Cursor's "too many computers" limit.

## v1.0.56

### Fixed

- **Tool calls sent in the same step no longer freeze the turn**:
  - Every exec sent to the IDE was named from the clock alone. On Windows the clock advances about once per millisecond, so two execs opened back to back got the same id: the second replaced the first as the awaited one, the first result was dropped as unmatched, and the turn waited until it was canceled. Exec ids are now unique within the process.
  - Applies to Cursor-account models and to BYOK models; both wait on the same table of pending execs.
- **`Delete` works on Cursor accounts**:
  - The tool card was created under the exec's id while the IDE looked it up by the request's tool call id, so every delete ended in `Failed to find tool call context: Timeout waiting for bubble creation`. The card now carries the request's id.
- **Cursor-account usage counts cached tokens once**:
  - Cursor reports a turn's input as the whole prompt, cache read and cache write included. Stored unchanged, those turns counted their cached tokens twice, showed about half their real cache hit rate and were priced as if the cached part had also been sent at the input rate. Input is now stored without cache tokens, as for every other provider; the IDE still receives the figures Cursor sent.
  - An existing `usage.json` is converted the first time this version writes to it. The file as it was is kept next to it as `usage.json.schema-2.bak`.
  - A turn's usage is no longer read back doubled while its provider call is still in the write buffer.
- **Max Mode keeps Cursor's default options**:
  - The model list served to the IDE marked a max-only variant as the Max Mode default: the larger context window for some models, Fast serving and the lowest effort for others. Cursor's own default is passed through now, so the larger window and Fast stay something you pick. A chat on a larger window re-reads that much more context on every tool step.
  - A request with Max Mode on and no explicit options is completed from Cursor's default as well, instead of from the first max-only variant.
  - Options already chosen in the model picker are remembered by the IDE per model and stay as they are.
- **A refusal from Cursor is shown as the refusal**:
  - When Cursor declines a call after part of the response went out (a billing notice, an exhausted limit), the message used to open with `upstream stream interrupted mid-response`. It now shows Cursor's own text.
- **Settings save reliably on Windows**:
  - A save no longer fails with `Access is denied` when the config file is briefly open elsewhere; the replace is retried for a moment.
  - Each save writes its own temporary file. Two parts of the app sharing `config.yaml.tmp` could fail each other's saves.
- **Vision delegation**: images described in parallel no longer share one request id in logs and debug artifacts.

## v1.0.55

### Fixed

- **`AwaitShell` rewritten to match the Cursor client's own tool**:
  - The shell's terminal file is now the source of truth: completion, exit code and run time come from its footer, `pattern` is matched against the command's output only, and the call really blocks until the command ends, the pattern matches or `block_until_ms` runs out.
  - Works on remote workspaces (WSL, SSH, containers), where the terminal file is read through the IDE. Output containing `---` lines no longer hides a finished command, and the file's header lines are no longer matched as output.
  - A wait now counts as outstanding work for the turn and is bounded only by its own `block_until_ms`: watchdogs no longer answer it early, and a cancel or a new turn drops it cleanly.
  - When a `Shell` command moves to the background, the result the model reads now carries the output collected so far and the terminals folder that holds the rest.
- **Long-running commands no longer end the turn with `pending exec not found`**:
  - A foreground shell is no longer replaced by a synthetic result about 30 seconds after it was requested. The foreground window now starts when the command starts running (not while it waits for approval), honors the `timeout` of Cursor-account shells, and is extended by up to 2 minutes while the IDE still heartbeats for the command.
  - The turn-stale watchdog no longer force-completes a turn while the IDE is still reporting in on a pending tool; heartbeats are now tracked for every tool kind, not only shells.
  - Results and acknowledgements that arrive for an exec that was already closed (after a recovery, a cancel or a new turn) are recognized and dropped instead of failing the turn.
- **Each chat keeps its own shell**:
  - The IDE keeps a shell's working directory and environment per conversation id. Shell commands from BYOK models carried no id, so every chat and subagent in a window shared one shell; they now carry the chat's id.
  - Cursor-account shells are pinned to the local chat's id, so the shell no longer resets to the workspace root when a turn continues on a fresh upstream conversation.
- **Chats that cursor-byok has no history for can be continued**:
  - Todos, the plan and the summary that the IDE sends as blob references are now resolved instead of failing every turn with `decode imported todo: proto: cannot parse invalid wire-format data`; a reference that cannot be found is dropped.
- **Checkpoints and message ordering**:
  - A checkpoint blob that the IDE confirms late no longer fails a running turn with `checkpoint_sync_error`; mid-turn it is skipped and retried.
  - History entries appended while a checkpoint was being written are no longer counted as saved without reaching the store.
  - The append sequencer keeps its place for a request that is still running; after 10 quiet minutes, later IDE messages could previously wait on one that would never arrive.

### Improved

- **Faster Cursor-account turns**:
  - Replies to Cursor's servers (tool results, blob acknowledgements) now travel side by side in a 32-frame window, as the IDE's own transport sends them, instead of one round trip at a time.
  - The agent host is remembered for 10 minutes per credential instead of being looked up before every model pass.
- **Less work per tool call for every model**:
  - Checkpoint requests inside the send interval are deferred without building the checkpoint, the metadata sync after each tool result moved off the stream's actor into the debounced checkpoint persist, and client heartbeats are no longer written to history.
  - The local proxy keeps up to 64 idle connections to the backend instead of 2, so bursts of IDE messages no longer reopen connections.

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

- **Windows 64 位（绝大多数 Windows 电脑）**：下载 `cursor-byok-1.0.63-windows-x64-installer.exe`（安装版，推荐）或 `cursor-byok-1.0.63-windows-x64.zip`（绿色版）
- **Windows ARM64（骁龙/麒麟等 ARM 处理器的 Windows 电脑）**：下载 `cursor-byok-1.0.63-windows-arm64-installer.exe` 或 `cursor-byok-1.0.63-windows-arm64.zip`
- **Windows 32 位（很老的低配电脑才需要）**：下载 `cursor-byok-1.0.63-windows-x32-installer.exe` 或 `cursor-byok-1.0.63-windows-x32.zip`
- **macOS Apple Silicon（M1/M2/M3/M4 芯片）**：下载 `cursor-byok-1.0.63-macos-arm64.dmg` 或 `cursor-byok-1.0.63-macos-arm64.tar.gz`
- **macOS Intel**：下载 `cursor-byok-1.0.63-macos-x64.dmg` 或 `cursor-byok-1.0.63-macos-x64.tar.gz`
- **Linux 64 位**：下载 `cursor-byok-1.0.63-linux-x64.tar.gz`

Windows 系统类型可在「设置 → 系统 → 关于 → 系统类型」查看；macOS 在「关于本机」查看芯片或处理器。
