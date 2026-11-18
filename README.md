# Cursor Assistant

<p align="center">
  <img src="frontend/src/assets/logo.png" width="96" alt="Cursor Assistant logo" />
</p>

<h1 align="center">Cursor Assistant</h1>

<p align="center">A local model adapter, proxy gateway, usage dashboard, and agent workspace for Cursor.</p>

<p align="center">
  <a href="https://github.com/4cuck/cursor-byok/releases"><img src="https://img.shields.io/github/v/release/4cuck/cursor-byok?display_name=tag&logo=github&label=Release" alt="Latest release" /></a>
  <a href="https://github.com/4cuck/cursor-byok/actions/workflows/build.yml"><img src="https://img.shields.io/github/actions/workflow/status/4cuck/cursor-byok/build.yml?logo=githubactions&label=Build" alt="Build status" /></a>
  <a href="https://github.com/4cuck/cursor-byok/blob/main/LICENSE"><img src="https://img.shields.io/github/license/4cuck/cursor-byok?logo=opensourceinitiative&label=License" alt="License" /></a>
  <a href="https://go.dev/"><img src="https://img.shields.io/badge/Go-1.25-00ADD8?logo=go&logoColor=white" alt="Go 1.25" /></a>
</p>

<p align="center">
  <a href="#quick-start">Quick start</a> ·
  <a href="#what-it-does">What it does</a> ·
  <a href="#model-setup">Model setup</a> ·
  <a href="#preview">Preview</a> ·
  <a href="#community">Community</a>
</p>

<p align="center">
  <img src="docs/assets/home-preview.png" width="960" alt="Cursor Assistant home screen" />
</p>

<p align="center">
  <img src="docs/assets/model-config-preview.png" width="960" alt="Cursor Assistant model configuration" />
</p>

> Connect your own model APIs to Cursor. Provider setup, protocol adaptation, routing, usage stats, delegation, and diagnostics all stay on your machine.

This repository is a fork of [Sakana-yuyu/cursor-byok](https://github.com/Sakana-yuyu/cursor-byok). Releases for this fork are published from [4cuck/cursor-byok](https://github.com/4cuck/cursor-byok/releases).

## Why use it

Cursor stays the editor and agent. Cursor Assistant sits in front of the model connection: you keep Cursor's tools and task flow, and you choose whether a request goes to an official API, a compatible gateway, or a service you run yourself.

## What it does

| | Capability | What you get |
| :---: | --- | --- |
| <img src="frontend/public/supplier-icons/openai.svg" width="22" height="22" alt="OpenAI" /> | Several providers | OpenAI-compatible APIs, native Anthropic, native Gemini, and common third-party gateways. |
| <img src="https://cdn.simpleicons.org/serverless/10AD5D" width="22" alt="Gateway" /> | Local proxy | Request forwarding and streaming translation between Cursor and the provider, on this computer. |
| <img src="https://cdn.simpleicons.org/googleanalytics/4285F4" width="22" alt="Metrics" /> | Usage and cost | Request counts, input and output tokens, cache hit rate, per-site spend, and an estimated cost. |
| <img src="https://cdn.simpleicons.org/robotframework/10AD5D" width="22" alt="Agent" /> | Agent and multitask | Explorer subagents, vision delegation, supervision state, and Cursor task status. |
| <img src="https://cdn.simpleicons.org/icloud/4F46E5" width="22" alt="Catalog" /> | Model catalog | Pull a provider's model list, match context windows, and probe whether a model responds. |
| <img src="https://cdn.simpleicons.org/bugcrowd/F59E0B" width="22" alt="Debug" /> | Diagnostics | Redacted request traces, provider passes, tool calls, and final status. |
| <img src="frontend/public/supplier-icons/privacy-shield.svg" width="22" height="22" alt="Privacy" /> | Local credentials | API keys stay in the local config. Published source does not contain your keys. |
| <img src="https://cdn.simpleicons.org/rotate/10AD5D" width="22" alt="Rotation" /> | Key rotation | Extra keys for one model rotate independently, with their own cooldown after a failure. |
| <img src="https://cdn.simpleicons.org/shieldcheck/10AD5D" width="22" alt="Availability" /> | Availability guard | Optional "disable after a failed test" keeps a broken model out of Cursor until a later test succeeds. |

### Model setup

The model page is a list on the left and a detail panel on the right.

- **Groups.** Group by name or by connection. Search, pull a catalog, and run a diagnosis from the same page.
- **Tests.** Test one model or a whole group. The card shows whether it answered, and how long the first response and first token took.
- **Extra keys.** In the model editor, put one backup key per line. The primary key and the backups are separate candidates.
- **Disable after failure.** Turn this on at the top of the page. A failed model is marked disabled, left out of Cursor's list, and left out of routing. A later successful test, or Enable, brings it back.
- **Diagnostics and stats.** The sidebar opens protocol and channel health, and a per-request view of tokens, cache, and timing.

### Platforms

- Windows 64-bit, Windows ARM64, and Windows 32-bit
- Linux 64-bit
- macOS Intel and macOS Apple Silicon

GitHub Actions builds the packages. Download the file for your system from [Releases](https://github.com/4cuck/cursor-byok/releases).

## Preview

### One-minute demo

<video controls muted loop playsinline poster="docs/assets/home-preview.png" width="960">
  <source src="docs/assets/cursor-assistant-demo.mp4" type="video/mp4" />
  This browser cannot play the embedded video. Open the <a href="docs/assets/cursor-assistant-demo.mp4">demo video</a> directly.
</video>

### GIF

![Cursor Assistant configuration and stats](docs/assets/cursor-assistant-demo.gif)

The video and GIF are the browser preview. The data is local mock data. It does not call a real API and it does not contain real keys.

## Quick start

### 1. Install

1. Open [GitHub Releases](https://github.com/4cuck/cursor-byok/releases).
2. Download the installer or archive for your system.
3. If Windows SmartScreen interrupts the installer, choose More info, then Run anyway, after you confirm the file came from this release.
4. Start Cursor Assistant. The first launch creates a local MITM certificate in your user data directory. The private key is not part of the source tree.

### 2. Add a model and test it

1. Open Model configuration from the left navigation.
2. Use `+` at the top of the list, or add a model inside an existing provider.
3. Fill in the API address, API key, and model id. For failover, put one backup key per line.
4. Prefer Pull models, then pick from the provider catalog.
5. Press Test and confirm the model answers.
6. If you want broken models kept out of Cursor, turn on Disable after a failed test.
7. Save. A disabled model can be turned back on from its card.

> The address has to be the protocol endpoint that provider actually uses. Some want `/v1`, some want Anthropic's native path, Gemini wants `/v1beta`, and a gateway may use its own path. If the model list is empty, check the address, the auth header, and the catalog URL first.

### 3. Point Cursor at the local proxy

1. Start the local service from the home page.
2. Press Repair Cursor configuration so the app writes the local proxy address.
3. Quit Cursor completely and open it again.
4. Send a normal request in Cursor and confirm the reply and tool calls both work.

### 4. Multitask and vision delegation

1. Turn on Multitask in settings.
2. Pick a working model for Explorer or vision delegation.
3. Run a normal exploration task in Cursor. Cursor Assistant creates the delegation from that configuration.
4. Watch status, tokens, and cache in session analysis or the live stats overlay.

### 5. Balance and usage

- Provider detail shows the catalog, test results, availability, and provider balance.
- The home page shows request volume, tokens, cache hit rate, per-site spend, and estimated cost.
- Session analysis shows the model, provider, duration, final status, cache, and errors for each request.
- Diagnostics shows protocol configuration, channel health, cooldown, and the trace you can follow.
- Check exported logs before you share them. They can contain your own prompt text or a provider response.

## Common problems

### Cursor says stopped, and the app shows no result

Keep the request time, Cursor version, model id, task text, and the runtime or debug log under the app data directory. The useful ids are `request_id`, `conversation_id`, `model_call_id`, `tool_call_id`, and `exec_id`. Those should connect one execution from start to finish.

### The model list is empty, or the provider returns 401 or 404

Check these in order:

1. The API address includes the version path that provider expects.
2. The auth header matches that provider.
3. The provider actually exposes `/models` or another catalog endpoint.
4. The model id belongs to this account or this gateway.
5. A Gemini or Anthropic native endpoint was not filled in as an OpenAI-compatible URL.

### Windows or macOS certificate prompt

Cursor Assistant creates a CA in the user data directory and can ask the system to trust it. Do not upload `ca.key`, an API key, or a full request log to an issue, a file host, or git.

### How to delete local secrets

Quit Cursor and Cursor Assistant first. Then delete the config, logs, history, and `data/ca.key` in the app data directory. The next launch creates a new local CA.

## Develop and verify

The backend is Go. The desktop UI is in `frontend`. Protocol definitions are in `proto`.

    go test ./...
    go vet ./...
    go build ./...

    Set-Location frontend
    yarn install --frozen-lockfile
    yarn lint
    node ./scripts/run-vite-build.mjs --scan --mode production

Browser preview:

    Set-Location frontend
    yarn dev:browser

Before a release, also check that secrets and build output are not tracked:

    git status --short
    git ls-files | Select-String -Pattern '(^|/)(\.env|.*\.key|.*\.pem|logs?|dist|bin|node_modules)(/|$)'

## Security and privacy

- Do not commit API keys, access tokens, cookies, private keys, user databases, full request logs, or local history.
- `internal/certs/ca.key` belongs in the user data directory only. It is not part of the source distribution.
- Debug logs can contain prompt text, model names, and provider metadata. Redact them before you share a log.
- Billing, balance, and data retention are the provider's policy. Read their terms yourself.
- This project only adapts and forwards requests on your machine. It does not host your API keys, and it cannot promise that a third-party provider stays up.

## Community

- [This fork](https://github.com/4cuck/cursor-byok)
- [Releases](https://github.com/4cuck/cursor-byok/releases)
- [Upstream repository](https://github.com/Sakana-yuyu/cursor-byok)
- [Linux.do](https://linux.do/)
- [Original author's Bilibili page](https://space.bilibili.com/311706663/upload/video)

The Bilibili link is the original author's public page, kept for attribution. Code, releases, and issues for this fork are handled in this repository.

## Contributing

When you open an issue or a pull request, include:

1. Steps to reproduce.
2. Operating system, Cursor version, and Cursor Assistant version.
3. Provider type and model id. Do not include an API key.
4. The error text and the smallest log excerpt that still shows the failure, with secrets removed.

Read `CONTRIBUTING.md` and `CONTRIBUTING_EN.md` first.

## License

Released under the [MIT License](LICENSE).

<!-- contributors-start -->
<table><tr>
<td><a href="https://github.com/4cuck/cursor-byok"><img src="https://avatars.githubusercontent.com/u/20703959?v=4&s=80" width="48" height="48" alt="Sakana-yuyu" title="Sakana-yuyu (534 次提交)"/></a></td>
<td><a href="https://github.com/4cuck/cursor-byok"><img src="https://secure.gravatar.com/avatar/e91e20e8d5f83234900a3878086e1fe7?d=identicon&s=80" width="48" height="48" alt="呆呆可达鸭鸭" title="呆呆可达鸭鸭 (236 次提交)"/></a></td>
<td><a href="https://github.com/4cuck/cursor-byok"><img src="https://avatars.githubusercontent.com/u/41898282?v=4&s=80" width="48" height="48" alt="github-actions[bot]" title="github-actions[bot] (74 次提交)"/></a></td>
<td><a href="https://github.com/4cuck/cursor-byok"><img src="https://avatars.githubusercontent.com/u/131544788?v=4&s=80" width="48" height="48" alt="leookun" title="leookun (18 次提交)"/></a></td>
<td><a href="https://github.com/4cuck/cursor-byok"><img src="https://avatars.githubusercontent.com/u/40539855?v=4&s=80" width="48" height="48" alt="4cuck" title="4cuck (9 次提交)"/></a></td>
<td><a href="https://github.com/4cuck/cursor-byok"><img src="https://secure.gravatar.com/avatar/e32a23a62c503dd189268d84dbd12c2d?d=identicon&s=80" width="48" height="48" alt="hudawang" title="hudawang (7 次提交)"/></a></td>
<td><a href="https://github.com/4cuck/cursor-byok"><img src="https://secure.gravatar.com/avatar/4fa4116634f7a272554140a170e7e300?d=identicon&s=80" width="48" height="48" alt="DedSecer" title="DedSecer (5 次提交)"/></a></td>
<td><a href="https://github.com/4cuck/cursor-byok"><img src="https://secure.gravatar.com/avatar/92ab25d105c69286f299bdc514bef2ca?d=identicon&s=80" width="48" height="48" alt="philau2512" title="philau2512 (3 次提交)"/></a></td>
<td><a href="https://github.com/kael-odin"><img src="https://avatars.githubusercontent.com/kael-odin?v=4&s=80" width="48" height="48" alt="kael-odin" title="kael-odin (3 次提交)"/></a></td>
<td><a href="https://github.com/4cuck/cursor-byok"><img src="https://secure.gravatar.com/avatar/3f43639246884941cd37c80aaf1c8293?d=identicon&s=80" width="48" height="48" alt="上玄" title="上玄 (2 次提交)"/></a></td>
<td><a href="https://github.com/4cuck/cursor-byok"><img src="https://secure.gravatar.com/avatar/2d94f41c79230c6c7afb2023d8250167?d=identicon&s=80" width="48" height="48" alt="杨超" title="杨超 (1 次提交)"/></a></td>
<td><a href="https://github.com/4cuck/cursor-byok"><img src="https://avatars.githubusercontent.com/u/266937838?v=4&s=80" width="48" height="48" alt="octo-patch" title="octo-patch (1 次提交)"/></a></td>
<td><a href="https://github.com/4cuck/cursor-byok"><img src="https://secure.gravatar.com/avatar/a86b8f4b14ce67e4e6a4f3b25612e99c?d=identicon&s=80" width="48" height="48" alt="lixingcheng" title="lixingcheng (1 次提交)"/></a></td>
<td><a href="https://github.com/4cuck/cursor-byok"><img src="https://secure.gravatar.com/avatar/edd1b95493e930ffec5730df1d8ae4d7?d=identicon&s=80" width="48" height="48" alt="lixiangwuxian" title="lixiangwuxian (1 次提交)"/></a></td>
<td><a href="https://github.com/4cuck/cursor-byok"><img src="https://secure.gravatar.com/avatar/eaa039f995d17d9c5bc80586f4523276?d=identicon&s=80" width="48" height="48" alt="aike1202" title="aike1202 (1 次提交)"/></a></td>
<td><a href="https://github.com/4cuck/cursor-byok"><img src="https://secure.gravatar.com/avatar/3972eaf41431b5a058211d1262fbd2a3?d=identicon&s=80" width="48" height="48" alt="TigerWang" title="TigerWang (1 次提交)"/></a></td>
<td><a href="https://github.com/4cuck/cursor-byok"><img src="https://secure.gravatar.com/avatar/38f67cb0e7464d31dc810917801e61ae?d=identicon&s=80" width="48" height="48" alt="GGHansome" title="GGHansome (1 次提交)"/></a></td>
</tr></table>
<!-- contributors-end -->
