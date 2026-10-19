# Installs the orch CLI on Windows.
#
#   irm https://raw.githubusercontent.com/TorstenDittmann/orch/main/install.ps1 | iex
#
# $env:ORCH_VERSION picks a release tag such as v0.2.0 (default: the latest
# release). $env:ORCH_INSTALL_DIR picks the directory (default:
# %LOCALAPPDATA%\Programs\orch), which is added to the user PATH.
$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'
[Net.ServicePointManager]::SecurityProtocol = [Net.ServicePointManager]::SecurityProtocol -bor [Net.SecurityProtocolType]::Tls12

$repo = 'TorstenDittmann/orch'
$version = if ($env:ORCH_VERSION) { $env:ORCH_VERSION } else { 'latest' }

$machine = if ($env:PROCESSOR_ARCHITEW6432) { $env:PROCESSOR_ARCHITEW6432 } else { $env:PROCESSOR_ARCHITECTURE }
switch ($machine) {
    'AMD64' { $arch = 'amd64' }
    'ARM64' { $arch = 'arm64' }
    default { throw "orch install: unsupported architecture $machine" }
}

$asset = "orch-windows-$arch.exe"
if ($version -eq 'latest') {
    $base = "https://github.com/$repo/releases/latest/download"
} else {
    if (-not $version.StartsWith('v')) { $version = "v$version" }
    $base = "https://github.com/$repo/releases/download/$version"
}

$tmp = Join-Path ([IO.Path]::GetTempPath()) ("orch-" + [Guid]::NewGuid())
New-Item -ItemType Directory -Path $tmp | Out-Null
try {
    $binary = Join-Path $tmp 'orch.exe'
    $sums = Join-Path $tmp 'SHA256SUMS'
    Invoke-WebRequest -UseBasicParsing -Uri "$base/SHA256SUMS" -OutFile $sums
    $checksums = @{}
    foreach ($line in Get-Content $sums) {
        $fields = $line -split '\s+'
        if ($fields.Count -ge 2) { $checksums[$fields[1].TrimStart('*')] = $fields[0] }
    }
    # The gzipped asset is about a third of the size; releases before it
    # existed only have the plain binary.
    if ($checksums.ContainsKey("$asset.gz")) { $asset = "$asset.gz" }
    $expected = $checksums[$asset]
    if (-not $expected) { throw "orch install: SHA256SUMS has no entry for $asset" }

    Write-Host "Downloading $asset ($version)"
    $download = Join-Path $tmp $asset
    Invoke-WebRequest -UseBasicParsing -Uri "$base/$asset" -OutFile $download
    $actual = (Get-FileHash -Algorithm SHA256 -Path $download).Hash
    if ($actual -ne $expected) { throw "orch install: checksum mismatch for $asset" }
    if ($asset.EndsWith('.gz')) {
        $in = [IO.File]::OpenRead($download)
        $out = [IO.File]::Create($binary)
        try {
            $gzip = New-Object IO.Compression.GZipStream($in, [IO.Compression.CompressionMode]::Decompress)
            $gzip.CopyTo($out)
        } finally {
            $out.Dispose()
            $in.Dispose()
        }
    } else {
        Move-Item -Force $download $binary
    }

    $dir = if ($env:ORCH_INSTALL_DIR) { $env:ORCH_INSTALL_DIR } else { Join-Path $env:LOCALAPPDATA 'Programs\orch' }
    New-Item -ItemType Directory -Force -Path $dir | Out-Null
    $target = Join-Path $dir 'orch.exe'
    # A running orch.exe cannot be overwritten but can be renamed out of the way.
    if (Test-Path $target) {
        $old = "$target.old"
        Remove-Item -Force $old -ErrorAction SilentlyContinue
        Move-Item -Force $target $old
    }
    Move-Item -Force $binary $target
    Remove-Item -Force "$target.old" -ErrorAction SilentlyContinue
} finally {
    Remove-Item -Recurse -Force $tmp -ErrorAction SilentlyContinue
}

$userPath = [Environment]::GetEnvironmentVariable('Path', 'User')
$entries = @($userPath -split ';' | Where-Object { $_ })
if ($entries -notcontains $dir) {
    [Environment]::SetEnvironmentVariable('Path', (($entries + $dir) -join ';'), 'User')
    Write-Host "Added $dir to your PATH"
}
if (@($env:Path -split ';') -notcontains $dir) { $env:Path = "$env:Path;$dir" }

$installed = & $target version 2>$null
Write-Host "Installed orch $installed to $target"
Write-Host ''
Write-Host "Next: run 'orch target add NAME ssh://root@HOST' to manage a cluster."
