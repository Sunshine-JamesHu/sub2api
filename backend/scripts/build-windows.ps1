param(
    [string]$LlvmRoot = 'E:\toolchains\llvm-mingw-20261020-ucrt-x86_64',
    [string]$SpiderMonkeyRoot = 'E:\toolchains\spidermonkey',
    [string]$Output = 'bin\server-playground-windows-amd64.exe',
    [switch]$WithoutPlayground,
    [switch]$DownloadDependencies,
    [switch]$SkipFrontend
)
$ErrorActionPreference = 'Stop'
$backend = Split-Path -Parent $PSScriptRoot
$spiderMonkeyVersion = 'v152.0.4-aistudio2api.1'
$llvmVersion = '20261020'
$previous = @{}
foreach ($key in @('CGO_ENABLED', 'GOOS', 'GOARCH', 'CC', 'CXX', 'CGO_CXXFLAGS', 'CGO_LDFLAGS', 'TEMP', 'TMP', 'GOTMPDIR')) {
    $previous[$key] = [Environment]::GetEnvironmentVariable($key, 'Process')
}
Push-Location $backend
try {
    $buildTemp = Join-Path $backend '.build-deps\tmp'
    New-Item -ItemType Directory -Force -Path $buildTemp | Out-Null
    $env:TEMP = $buildTemp
    $env:TMP = $buildTemp
    $env:GOTMPDIR = $buildTemp
    if (!$SkipFrontend) {
        $frontend = Join-Path (Split-Path -Parent $backend) 'frontend'
        if (!(Test-Path -LiteralPath (Join-Path $frontend 'node_modules\vite\bin\vite.js'))) {
            throw 'Install frontend dependencies first: pnpm --dir frontend install --frozen-lockfile'
        }
        Push-Location $frontend
        try {
            & node node_modules/vitest/vitest.mjs run src/i18n/__tests__/localeKeyCompleteness.spec.ts
            if ($LASTEXITCODE -ne 0) { throw 'Frontend locale check failed' }
            & node node_modules/vue-tsc/bin/vue-tsc.js -b
            if ($LASTEXITCODE -ne 0) { throw 'Frontend type check failed' }
            & node node_modules/vite/bin/vite.js build
            if ($LASTEXITCODE -ne 0) { throw 'Frontend build failed' }
        } finally { Pop-Location }
    }
    if (!(Test-Path -LiteralPath (Join-Path $backend 'internal\web\dist\index.html'))) {
        throw 'Embedded frontend is missing; build without -SkipFrontend'
    }
    if ($WithoutPlayground -and $Output -eq 'bin\server-playground-windows-amd64.exe') {
        $Output = 'bin\server-no-playground-windows-amd64.exe'
    }
    $env:GOOS = 'windows'
    $env:GOARCH = 'amd64'
    if ($WithoutPlayground) {
        $env:CGO_ENABLED = '0'
    } else {
        if ($DownloadDependencies) {
            $cache = Join-Path $backend '.build-deps'
            New-Item -ItemType Directory -Force -Path $cache | Out-Null
            $LlvmRoot = Join-Path $cache "llvm-mingw-$llvmVersion-ucrt-x86_64"
            $SpiderMonkeyRoot = Join-Path $cache "spidermonkey-$spiderMonkeyVersion-windows-amd64"
            if (!(Test-Path -LiteralPath $LlvmRoot)) {
                $archive = Join-Path $cache 'llvm-mingw.zip'
                Invoke-WebRequest "https://github.com/mstorsjo/llvm-mingw/releases/download/$llvmVersion/llvm-mingw-$llvmVersion-ucrt-x86_64.zip" -OutFile $archive
                Expand-Archive -LiteralPath $archive -DestinationPath $cache
            }
            if (!(Test-Path -LiteralPath $SpiderMonkeyRoot)) {
                $archive = Join-Path $cache 'spidermonkey.tar.gz'
                Invoke-WebRequest "https://github.com/Mag1cFall/firefox/releases/download/$spiderMonkeyVersion/spidermonkey-$spiderMonkeyVersion-windows-amd64.tar.gz" -OutFile $archive
                New-Item -ItemType Directory -Path $SpiderMonkeyRoot | Out-Null
                & tar -xzf $archive -C $SpiderMonkeyRoot
                if ($LASTEXITCODE -ne 0) { throw 'SpiderMonkey extraction failed' }
            }
        }
        $compiler = Join-Path $LlvmRoot 'bin\x86_64-w64-mingw32-clang.exe'
        $cxx = Join-Path $LlvmRoot 'bin\x86_64-w64-mingw32-clang++.exe'
        $header = Join-Path $SpiderMonkeyRoot 'include\js-config.h'
        $libraries = @((Join-Path $SpiderMonkeyRoot 'lib\libjs_static.a'), (Join-Path $SpiderMonkeyRoot 'lib\libjsrust.a'))
        foreach ($file in @($compiler, $cxx, $header, (Join-Path $SpiderMonkeyRoot 'include\jsapi.h')) + $libraries) {
            if (!(Test-Path -LiteralPath $file -PathType Leaf)) { throw "Missing build dependency: $file" }
        }
        if ((Get-Content -LiteralPath $header -Raw) -notmatch 'MOZJS_MAJOR_VERSION\s+152\b') {
            throw "Use SpiderMonkey $spiderMonkeyVersion"
        }
        & $compiler --version
        if ($LASTEXITCODE -ne 0) { throw 'LLVM-MinGW compiler failed' }
        $env:CGO_ENABLED = '1'
        $env:CC = $compiler.Replace('\', '/')
        $env:CXX = $cxx.Replace('\', '/')
        $include = (Join-Path $SpiderMonkeyRoot 'include').Replace('\', '/')
        $lib = (Join-Path $SpiderMonkeyRoot 'lib').Replace('\', '/')
        $env:CGO_CXXFLAGS = $(if ($include -match '\s') { '"-I' + $include + '"' } else { "-I$include" })
        $env:CGO_LDFLAGS = $(if ($lib -match '\s') { '"-L' + $lib + '"' } else { "-L$lib" })
    }
    & go build -mod=readonly -trimpath -tags embed -ldflags '-s -w' -o $Output ./cmd/server
    if ($LASTEXITCODE -ne 0) { throw 'Windows build failed' }
    if (!$WithoutPlayground) {
        & (Resolve-Path -LiteralPath $Output).Path --check-playground-runtime
        if ($LASTEXITCODE -ne 0) { throw 'SpiderMonkey runtime smoke check failed' }
    }
    $manifest = @{
        playground = !$WithoutPlayground
        spidermonkey = $(if ($WithoutPlayground) { $null } else { $spiderMonkeyVersion })
        llvm_mingw = $(if ($WithoutPlayground) { $null } else { $llvmVersion })
        binary_sha256 = (Get-FileHash -LiteralPath $Output -Algorithm SHA256).Hash
        libraries = $(if ($WithoutPlayground) { @() } else { @($libraries | ForEach-Object { @{ name = (Split-Path -Leaf $_); sha256 = (Get-FileHash -LiteralPath $_ -Algorithm SHA256).Hash } }) })
    }
    $manifest | ConvertTo-Json -Depth 4 | Set-Content -LiteralPath "$Output.build.json" -Encoding utf8
    Write-Host "Built: $((Resolve-Path -LiteralPath $Output).Path)"
} finally {
    Pop-Location
    foreach ($key in $previous.Keys) { [Environment]::SetEnvironmentVariable($key, $previous[$key], 'Process') }
}
