# Building and Releasing `cursor-byok` for All Platforms (Local Windows + Debian WSL)

This guide documents how to build the frontend and all 12 release artifacts (Windows `x64`/`arm64`/`x32`, macOS `arm64`/`x64`, Linux `x64`, plus `update.json`) locally from a Windows host using **native Windows Go/NSIS** and **Debian WSL2 (`osxcross` + `rcodesign` + `genisoimage`/`dmg`)**, without waiting on slow GitHub Actions runners.

---

## Release Artifacts Produced (12 files in `bin/release/<VER>/`)

| Platform | Artifacts |
|---|---|
| Windows x64 (`amd64`) | `cursor-byok-<VER>-windows-x64.zip`, `cursor-byok-<VER>-windows-x64-installer.exe` |
| Windows ARM64 (`arm64`) | `cursor-byok-<VER>-windows-arm64.zip`, `cursor-byok-<VER>-windows-arm64-installer.exe` |
| Windows x32 (`386`) | `cursor-byok-<VER>-windows-x32.zip`, `cursor-byok-<VER>-windows-x32-installer.exe` |
| macOS Apple Silicon (`arm64`) | `cursor-byok-<VER>-macos-arm64.tar.gz`, `cursor-byok-<VER>-macos-arm64.dmg` |
| macOS Intel (`amd64`) | `cursor-byok-<VER>-macos-x64.tar.gz`, `cursor-byok-<VER>-macos-x64.dmg` |
| Linux x64 (`amd64`) | `cursor-byok-<VER>-linux-x64.tar.gz` |
| Auto-updater manifest | `update.json` |

---

## Prerequisites

### Windows Host
- **Go** (`go1.26+`)
- **Node.js & npm** (for `frontend/` build and tests)
- **Wails v3 CLI** (`wails3` on `PATH`, used for `generate syso` and `generate webview2bootstrapper`)
- **NSIS** at `C:\Program Files (x86)\NSIS\makensis.exe`
- **GitHub CLI** (`gh`, authenticated with push/release access to `4cuck/cursor-byok`)

### Debian WSL2 (`wsl -d Debian -u root`)
- **Go**: `/usr/local/go/bin/go` (`go1.26+`)
- **Linux GUI build deps** (for `CGO_ENABLED=1` Wails Linux build):
  - `gcc`, `pkg-config`, `libgtk-3-dev` (`gtk+-3.0`), `libwebkit2gtk-4.1-dev` (`webkit2gtk-4.1`)
- **macOS cross-compilation toolchain**:
  - `osxcross` installed at `/opt/osxcross/target/bin` providing `arm64-apple-darwin23.5-clang{,++}` and `x86_64-apple-darwin23.5-clang{,++}`
  - `rcodesign` at `/usr/local/bin/rcodesign` (ad-hoc code signing for `.app` bundles)
  - `genisoimage` (`/usr/bin/genisoimage`) and `dmg` (`/usr/bin/dmg`, from `libdmg-hfsplus`) for building compressed `.dmg` images
  - `python3` (for syncing files to `/root/work/cursor-byok` and patching `Info.plist`)

Quick WSL toolchain check (from PowerShell):

```powershell
$s = @'
export PATH="/opt/osxcross/target/bin:/usr/local/go/bin:$PATH"
for t in go gcc pkg-config arm64-apple-darwin23.5-clang x86_64-apple-darwin23.5-clang rcodesign genisoimage dmg python3; do
  echo "$t => $(command -v "$t" || echo MISSING)"
done
go version
pkg-config --modversion gtk+-3.0 webkit2gtk-4.1
'@ -replace "`r`n", "`n"
$b64 = [Convert]::ToBase64String([System.Text.Encoding]::UTF8.GetBytes($s))
wsl -d Debian -u root -- bash -c "echo $b64 | base64 -d | bash"
```

> **Note on PowerShell + WSL quoting:** Always pass multi-line scripts to `wsl -d Debian -u root -- bash -c ...` via base64 (with `\r\n` normalized to `\n`) to avoid Windows CreateProcess quote-mangling and CRLF issues.

---

## Step-by-Step Release Process

### 1. Bump the Version Number

Update the version in all three files (replace `<VER>`, e.g. `1.0.53`):

1. `build/config.yml` → `info.version: "<VER>"`
2. `build/windows/info.json` → `fixed.file_version` and `info.0000.ProductVersion`
3. `release-notes.md` → add the new `## v<VER>` section at the top and update the download filenames at the bottom

Verify:

```powershell
go run ./scripts/release version
```

### 2. Run Tests and Build the Frontend

The compiled `frontend/dist/` bundle is embedded into all Go binaries (Windows, Linux, and macOS), so build it **before** running the Go cross-builds:

```powershell
# Go tests
go test ./internal/backend/agent/model/... ./internal/cursoraccount/... ./internal/client/...

# Frontend unit tests & production build
Push-Location frontend
npm run test:unit
if ($LASTEXITCODE -ne 0) { Pop-Location; throw "frontend tests failed" }
npm run build
if ($LASTEXITCODE -ne 0) { Pop-Location; throw "frontend build failed" }
Pop-Location
```

> If you changed Wails-bound Go service methods (`internal/bridge/`), regenerate bindings before `npm run build` via `wails3 generate bindings -clean=true`.

### 3. Commit and Push First (with `[skip ci]`)

Commit and push **before** compiling release binaries so `git rev-parse --short HEAD` embeds the exact pushed release commit hash into `cursor/internal/buildinfo.Commit`. Always include `[skip ci]` in the commit message so GitHub Actions does not trigger redundant CI/release builds:

```powershell
git status --short
# Stage only intended files (avoid untracked scratch files)
git add <files...>
git commit -m "feat(...): summary (v<VER>) [skip ci]"
git push origin main
```

### 4. Build All Windows Artifacts (`x64`, `arm64`, `x32` — ZIP + NSIS Installers)

Run from the repository root in PowerShell (set `$ver` to match `build/config.yml`):

```powershell
$ErrorActionPreference = 'Stop'
$ver = (go run ./scripts/release version).Trim()
$commit = (git rev-parse --short HEAD).Trim()
$releaseDir = "bin/release/$ver"
New-Item -ItemType Directory -Force -Path $releaseDir | Out-Null
$makensis = 'C:\Program Files (x86)\NSIS\makensis.exe'
$root = (Get-Location).Path
$ldflags = "-w -s -H windowsgui -X cursor/internal/buildinfo.Version=$ver -X cursor/internal/buildinfo.Commit=$commit"

Add-Type -AssemblyName System.IO.Compression.FileSystem

$targets = @(
    @{ GoArch = 'amd64'; Name = 'x64';   ArgFlag = 'AMD64'; Script = 'project.nsi';     TempBin = 'app-windows-amd64.exe' },
    @{ GoArch = 'arm64'; Name = 'arm64'; ArgFlag = 'ARM64'; Script = 'project.nsi';     TempBin = 'app-windows-arm64.exe' },
    @{ GoArch = '386';   Name = 'x32';   ArgFlag = '386';   Script = 'project-386.nsi'; TempBin = 'app-windows-x32.exe' }
)

foreach ($t in $targets) {
    $arch = $t.GoArch
    $name = $t.Name
    Write-Host "=== Building Windows $name ($arch) v$ver ($commit) ==="

    Remove-Item -Force -ErrorAction SilentlyContinue "wails_windows_*.syso"
    Push-Location build
    try {
        & wails3 generate syso -arch $arch -icon windows/icon.ico -manifest windows/wails.exe.manifest -info windows/info.json -out "../wails_windows_$arch.syso"
        if ($LASTEXITCODE -ne 0) { throw "wails3 generate syso failed for $arch" }
    } finally {
        Pop-Location
    }

    $exeRel = "bin/cursor-byok-windows-$name.exe"
    $env:GOOS = 'windows'
    $env:GOARCH = $arch
    $env:CGO_ENABLED = '0'
    & go build -tags production -trimpath -buildvcs=false -ldflags="$ldflags" -o $exeRel .
    $buildCode = $LASTEXITCODE
    Remove-Item -Force -ErrorAction SilentlyContinue "wails_windows_*.syso"
    if ($buildCode -ne 0) { throw "go build failed for windows/$arch" }

    # Create portable ZIP
    $stage = "bin/.zip-$arch"
    if (Test-Path $stage) { Remove-Item -Recurse -Force $stage }
    New-Item -ItemType Directory -Force -Path $stage | Out-Null
    Copy-Item -Force $exeRel $stage
    Copy-Item -Force THIRD_PARTY_NOTICES.md $stage
    $zipPath = Join-Path (Resolve-Path 'bin') "windows-$name.zip"
    if (Test-Path $zipPath) { Remove-Item -Force $zipPath }
    [System.IO.Compression.ZipFile]::CreateFromDirectory((Resolve-Path $stage), $zipPath)
    Remove-Item -Recurse -Force $stage
    Copy-Item -Force $zipPath "$releaseDir/cursor-byok-$ver-windows-$name.zip"

    # Build NSIS installer
    & wails3 generate webview2bootstrapper -dir "$root\build\windows\nsis"
    if ($LASTEXITCODE -ne 0) { throw "webview2bootstrapper failed" }
    Push-Location "$root\build\windows\nsis"
    try {
        Copy-Item -Force "$root\$exeRel" $t.TempBin
        Copy-Item -Force "$root\THIRD_PARTY_NOTICES.md" 'THIRD_PARTY_NOTICES.md'
        if ($t.Script -eq 'project-386.nsi') {
            & $makensis "-DINFO_PRODUCTVERSION=$ver" "-DARG_WAILS_386_BINARY=$($t.TempBin)" project-386.nsi
        } else {
            & $makensis "-DINFO_PRODUCTVERSION=$ver" "-DOUTPUT_INSTALLER=cursor-byok-windows-$name-installer.exe" "-DARG_WAILS_$($t.ArgFlag)_BINARY=$($t.TempBin)" project.nsi
        }
        if ($LASTEXITCODE -ne 0) { throw "makensis failed for $name" }
    } finally {
        Remove-Item -Force -ErrorAction SilentlyContinue $t.TempBin, 'THIRD_PARTY_NOTICES.md'
        Pop-Location
    }
    Copy-Item -Force "bin/cursor-byok-windows-$name-installer.exe" "$releaseDir/cursor-byok-$ver-windows-$name-installer.exe"
    Write-Host "=== Done Windows $name ==="
}

Remove-Item Env:GOOS, Env:GOARCH, Env:CGO_ENABLED -ErrorAction SilentlyContinue
```

> **Why copy binaries into `build/windows/nsis/` before `makensis`?** NSIS `File` instructions fail on mixed path separators or `..` segments; copying into the `.nsi` directory and cleaning up in `finally` avoids that.

### 5. Build Linux (`x64`) and macOS (`arm64` & `x64`) Artifacts via Debian WSL

The script incrementally syncs source + `frontend/dist` into WSL's native ext4 filesystem (`/root/work/cursor-byok`) for fast Go/CGO builds, compiles Linux `amd64` natively and macOS `arm64`/`amd64` via `osxcross`, ad-hoc signs `.app` bundles with `rcodesign`, packs `.tar.gz` updater archives and `.dmg` images (`genisoimage` + `dmg`), and copies the final artifacts back to `bin/release/<VER>/`.

> This step can run in parallel with Step 4 once `frontend/dist/` is built and committed.

```powershell
$ver = (go run ./scripts/release version).Trim()
$commit = (git rev-parse --short HEAD).Trim()

$script = @"
#!/usr/bin/env bash
set -euo pipefail
VER="$ver"
COMMIT="$commit"
APP_NAME="Cursor助手"
DMG_EXTRA="提示损坏？点我.command"
WIN_SRC="/mnt/c/Users/admin/Documents/cursor-byok"
WORK="/root/work/cursor-byok"
REL_OUT="`$WIN_SRC/bin/release/`$VER"
mkdir -p "`$REL_OUT"
echo "=== Syncing source to `$WORK (v`$VER @ `$COMMIT) ==="
mkdir -p "`$WORK"
python3 - "`$WIN_SRC" "`$WORK" << 'PY'
import os, shutil, sys
src, dst = sys.argv[1], sys.argv[2]
sync_dirs = ["internal", "build", "cmd", "gen", "prompt", "scripts", "frontend/dist", "frontend/bindings"]
sync_files = ["main.go", "go.mod", "go.sum", "THIRD_PARTY_NOTICES.md", "release-notes.md", "README.md"]
for rel in sync_files:
    s, d = os.path.join(src, rel), os.path.join(dst, rel)
    if os.path.exists(s):
        os.makedirs(os.path.dirname(d), exist_ok=True)
        shutil.copy2(s, d)
for rel in sync_dirs:
    s_root, d_root = os.path.join(src, rel), os.path.join(dst, rel)
    if not os.path.exists(s_root):
        continue
    src_files = set()
    for root, dirs, files in os.walk(s_root):
        rel_root = os.path.relpath(root, s_root)
        d_dir = d_root if rel_root == "." else os.path.join(d_root, rel_root)
        os.makedirs(d_dir, exist_ok=True)
        for f in files:
            sf = os.path.join(root, f)
            df = os.path.join(d_dir, f)
            src_files.add(os.path.relpath(sf, s_root))
            st_s = os.stat(sf)
            if not os.path.exists(df):
                shutil.copy2(sf, df)
            else:
                st_d = os.stat(df)
                if st_s.st_size != st_d.st_size or int(st_s.st_mtime) != int(st_d.st_mtime):
                    shutil.copy2(sf, df)
    for root, dirs, files in os.walk(d_root):
        for f in files:
            df = os.path.join(root, f)
            if os.path.relpath(df, d_root) not in src_files:
                os.remove(df)
print("Sync complete.")
PY
cd "`$WORK"
export PATH="/opt/osxcross/target/bin:/usr/local/go/bin:`$PATH"
LDFLAGS="-w -s -X cursor/internal/buildinfo.Version=`${VER} -X cursor/internal/buildinfo.Commit=`${COMMIT}"

echo "=== Building Linux x64 ==="
mkdir -p bin
GOOS=linux GOARCH=amd64 CGO_ENABLED=1 go build -tags production -trimpath -buildvcs=false -ldflags="`$LDFLAGS" -o bin/linux-x64 .
cp -f bin/linux-x64 "`$WIN_SRC/bin/cursor-byok-linux-x64"
LINUX_STAGE="/tmp/cursor-linux-stage"
rm -rf "`$LINUX_STAGE"
mkdir -p "`$LINUX_STAGE"
cp -f bin/linux-x64 "`$LINUX_STAGE/`$APP_NAME"
cp -f THIRD_PARTY_NOTICES.md "`$LINUX_STAGE/THIRD_PARTY_NOTICES.md"
chmod +x "`$LINUX_STAGE/`$APP_NAME"
env LC_ALL=C tar -C "`$LINUX_STAGE" -czf "/tmp/cursor-byok-`${VER}-linux-x64.tar.gz" "`$APP_NAME" THIRD_PARTY_NOTICES.md
cp -f "/tmp/cursor-byok-`${VER}-linux-x64.tar.gz" "`$REL_OUT/cursor-byok-`${VER}-linux-x64.tar.gz"
cp -f "/tmp/cursor-byok-`${VER}-linux-x64.tar.gz" "`$WIN_SRC/bin/linux-x64.tar.gz"
rm -rf "`$LINUX_STAGE" "/tmp/cursor-byok-`${VER}-linux-x64.tar.gz"
echo "=== Done Linux x64 ==="

build_macos() {
  local GOARCH="`$1" NAME="`$2" CC_BIN="`$3" CXX_BIN="`$4"
  echo "=== Building macOS `$NAME (`$GOARCH) ==="
  local OUT_BIN="/tmp/cursor-macos-`${NAME}"
  rm -f "`$OUT_BIN"
  GOOS=darwin GOARCH="`$GOARCH" CGO_ENABLED=1 CC="`$CC_BIN" CXX="`$CXX_BIN" \
    CGO_CFLAGS="-mmacosx-version-min=10.15" CGO_LDFLAGS="-mmacosx-version-min=10.15" MACOSX_DEPLOYMENT_TARGET="10.15" \
    go build -tags production -trimpath -buildvcs=false -ldflags="`$LDFLAGS" -o "`$OUT_BIN" .

  local BUNDLE_ROOT="/tmp/cursor-macos-bundle-`${NAME}"
  local APP_DIR="`$BUNDLE_ROOT/`${APP_NAME}.app"
  rm -rf "`$BUNDLE_ROOT"
  mkdir -p "`$APP_DIR/Contents/MacOS" "`$APP_DIR/Contents/Resources"
  cp -f "`$OUT_BIN" "`$APP_DIR/Contents/MacOS/`$APP_NAME"
  chmod +x "`$APP_DIR/Contents/MacOS/`$APP_NAME"
  cp -f build/darwin/icons.icns THIRD_PARTY_NOTICES.md "`$APP_DIR/Contents/Resources/"
  python3 - "`$WORK/build/darwin/Info.plist" "`$APP_DIR/Contents/Info.plist" "`$VER" << 'PY'
import plistlib, sys
with open(sys.argv[1], "rb") as f:
    pl = plistlib.load(f)
pl["CFBundleShortVersionString"] = sys.argv[3]
pl["CFBundleVersion"] = sys.argv[3]
with open(sys.argv[2], "wb") as f:
    plistlib.dump(pl, f)
PY

  rcodesign sign "`$APP_DIR"

  local TAR_TMP="/tmp/cursor-byok-`${VER}-macos-`${NAME}.tar.gz"
  rm -f "`$TAR_TMP"
  env LC_ALL=C tar -C "`$BUNDLE_ROOT" -czf "`$TAR_TMP" "`${APP_NAME}.app"
  cp -f "`$TAR_TMP" "`$REL_OUT/cursor-byok-`${VER}-macos-`${NAME}.tar.gz"
  cp -f "`$TAR_TMP" "`$WIN_SRC/bin/macos-`${NAME}.tar.gz"

  local DMG_STAGE="/tmp/cursor-dmg-stage-`${NAME}"
  local ISO_TMP="/tmp/cursor-macos-`${NAME}.iso"
  local DMG_TMP="/tmp/cursor-byok-`${VER}-macos-`${NAME}.dmg"
  rm -rf "`$DMG_STAGE" "`$ISO_TMP" "`$DMG_TMP"
  mkdir -p "`$DMG_STAGE"
  cp -a "`$APP_DIR" "`$DMG_STAGE/`${APP_NAME}.app"
  ln -s /Applications "`$DMG_STAGE/Applications"
  cp -f "build/dmg-extras/`$DMG_EXTRA" "`$DMG_STAGE/`$DMG_EXTRA"
  chmod +x "`$DMG_STAGE/`$DMG_EXTRA"
  genisoimage -V "`$APP_NAME" -D -R -apple -no-pad -o "`$ISO_TMP" "`$DMG_STAGE"
  dmg "`$ISO_TMP" "`$DMG_TMP"
  cp -f "`$DMG_TMP" "`$REL_OUT/cursor-byok-`${VER}-macos-`${NAME}.dmg"
  cp -f "`$DMG_TMP" "`$WIN_SRC/bin/macos-`${NAME}.dmg"

  rm -rf "`$OUT_BIN" "`$BUNDLE_ROOT" "`$DMG_STAGE" "`$ISO_TMP" "`$TAR_TMP" "`$DMG_TMP"
  echo "=== Done macOS `$NAME ==="
}

build_macos arm64 arm64 arm64-apple-darwin23.5-clang arm64-apple-darwin23.5-clang++
build_macos amd64 x64 x86_64-apple-darwin23.5-clang x86_64-apple-darwin23.5-clang++

echo "=== Verifying archive contents ==="
tar -tzf "`$REL_OUT/cursor-byok-`${VER}-linux-x64.tar.gz"
tar -tzf "`$REL_OUT/cursor-byok-`${VER}-macos-arm64.tar.gz" | head
tar -tzf "`$REL_OUT/cursor-byok-`${VER}-macos-x64.tar.gz" | head
"@ -replace "`r`n", "`n"

$b64 = [Convert]::ToBase64String([System.Text.Encoding]::UTF8.GetBytes($script))
wsl -d Debian -u root -- bash -c "echo '$b64' | base64 -d > /tmp/build-wsl-release.sh && chmod +x /tmp/build-wsl-release.sh && /tmp/build-wsl-release.sh && rm -f /tmp/build-wsl-release.sh"
```

### 6. Generate `update.json`, Validate All 12 Assets, and Publish the GitHub Release

> **Important:** Pass `-repo 4cuck/cursor-byok` explicitly (`Taskfile.yml` still points to the upstream `Sakana-yuyu/cursor-byok` default).

```powershell
$ErrorActionPreference = 'Stop'
$ver = (go run ./scripts/release version).Trim()
$tag = "v$ver"
$repo = '4cuck/cursor-byok'
$relDir = "bin/release/$ver"

# Generate update.json (includes legacy platform aliases linux-amd64, macos-amd64, windows-amd64, windows-386)
go run ./scripts/release manifest -config ./build/config.yml -assets-dir $relDir -out "$relDir/update.json" -repo $repo -base-name "cursor-byok" -notes "release-notes.md"
if ($LASTEXITCODE -ne 0) { throw "manifest generation failed" }

# Validate manifest keys, sizes, and URLs
$json = Get-Content -Raw "$relDir/update.json" | ConvertFrom-Json
$expectedKeys = @("linux-amd64","linux-x64","macos-amd64","macos-arm64","macos-x64","windows-386","windows-amd64","windows-arm64","windows-x32","windows-x64")
$actualKeys = @($json.platforms.PSObject.Properties.Name | Sort-Object)
if ($json.version -ne $ver) { throw "version mismatch: $($json.version)" }
if (($actualKeys -join ',') -ne ($expectedKeys -join ',')) { throw "platform keys mismatch: $($actualKeys -join ',')" }
foreach ($k in $actualKeys) {
    $p = $json.platforms.$k
    if ($p.size -le 0) { throw "empty size for $k" }
    if ($p.url.EndsWith('.dmg')) { throw "dmg url in update.json for $k" }
}

$assets = @(Get-ChildItem $relDir -File | ForEach-Object { $_.FullName })
if ($assets.Count -ne 12) { throw "expected 12 assets, got $($assets.Count)" }

# Publish GitHub Release
& gh release create $tag @assets --repo $repo --target main --title $tag --notes-file release-notes.md --latest
if ($LASTEXITCODE -ne 0) { throw "gh release create failed" }

gh release view $tag --repo $repo --json tagName,url,assets --jq '{tagName, url, assetCount: (.assets | length)}'
```

---

## Troubleshooting

- **WSL build slow or failing on `/mnt/c/...`:** Never run `go build` directly inside `/mnt/c/...` (9P filesystem overhead + permission/symlink quirks). Always sync to `/root/work/cursor-byok` first as shown above.
- **macOS `Info.plist` version out of date:** The WSL script patches `CFBundleShortVersionString` and `CFBundleVersion` dynamically via `python3` `plistlib`, so `build/darwin/Info.plist` does not need manual version edits.
- **Missing `wails3` or `osxcross`:** Verify `wails3` is on Windows `PATH` (`%USERPROFILE%\go\bin`) and `/opt/osxcross/target/bin` + `/usr/local/go/bin` are exported inside WSL.
- **Leftover `.syso` files:** Always delete `wails_windows_*.syso` between architectures; a leftover `amd64` `.syso` will break `arm64`/`386` linking.
