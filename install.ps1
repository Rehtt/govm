# Compatible with Windows PowerShell 5.1 and PowerShell 7.
$ErrorActionPreference = 'Stop'
$tempDir = $null
$staged = $null
$oldProtocol = [Net.ServicePointManager]::SecurityProtocol
try {
    if ([Environment]::OSVersion.Platform -ne [PlatformID]::Win32NT) {
        throw 'This script supports Windows only; use install.sh on Linux/macOS.'
    }
    $architecture = $env:PROCESSOR_ARCHITEW6432
    if (-not $architecture) { $architecture = $env:PROCESSOR_ARCHITECTURE }
    switch ($architecture) {
        'AMD64' { $arch = 'amd64' }
        'ARM64' { $arch = 'arm64' }
        default { throw "Unsupported architecture: $architecture (expected amd64 or arm64)." }
    }
    $installDir = $env:GOVM_INSTALL_DIR
    if (-not $installDir) { $installDir = Join-Path $HOME '.govm\bin' }
    $installDir = [IO.Path]::GetFullPath($installDir)
    if ($installDir -match '[;\r\n]') { throw 'Installation directory cannot contain semicolons or newlines.' }
    $tempDir = Join-Path ([IO.Path]::GetTempPath()) ('govm-install-' + [Guid]::NewGuid())
    $null = New-Item -ItemType Directory -Path $tempDir
    [Net.ServicePointManager]::SecurityProtocol = $oldProtocol -bor [Net.SecurityProtocolType]::Tls12
    $version = $env:GOVM_VERSION
    if (-not $version) {
        $release = Invoke-RestMethod -Uri 'https://api.github.com/repos/Rehtt/govm/releases/latest'
        $version = $release.tag_name
    }
    if (-not $version -or $version -notmatch '^[a-zA-Z0-9._-]+$') { throw 'Invalid Release tag.' }
    $asset = "govm_windows_$arch.zip"
    $base = "https://github.com/Rehtt/govm/releases/download/$version"
    Write-Host "Downloading govm $version (windows/$arch)..."
    $archive = Join-Path $tempDir $asset
    $checksumFile = Join-Path $tempDir 'checksums.txt'
    Invoke-WebRequest -UseBasicParsing -Uri "$base/$asset" -OutFile $archive
    Invoke-WebRequest -UseBasicParsing -Uri "$base/checksums.txt" -OutFile $checksumFile
    $entries = @(Get-Content -LiteralPath $checksumFile | Where-Object {
        $_ -match ('^([0-9a-fA-F]{64})\s+\*?' + [regex]::Escape($asset) + '\s*$')
    })
    if ($entries.Count -ne 1) { throw 'Missing or ambiguous checksum entry; existing installation preserved.' }
    $expected = ($entries[0] -split '\s+')[0]
    if ((Get-FileHash -LiteralPath $archive -Algorithm SHA256).Hash -ne $expected) {
        throw 'SHA-256 mismatch; existing installation preserved.'
    }
    $unpacked = Join-Path $tempDir 'unpacked'
    Expand-Archive -LiteralPath $archive -DestinationPath $unpacked
    $binary = Join-Path $unpacked 'govm.exe'
    if (-not (Test-Path -LiteralPath $binary -PathType Leaf)) { throw 'Archive does not contain govm.exe.' }
    $null = New-Item -ItemType Directory -Path $installDir -Force
    $target = Join-Path $installDir 'govm.exe'
    $staged = Join-Path $installDir ('.govm-install-' + [Guid]::NewGuid() + '.exe')
    Copy-Item -LiteralPath $binary -Destination $staged
    try {
        if (Test-Path -LiteralPath $target) { [IO.File]::Replace($staged, $target, $null) }
        else { [IO.File]::Move($staged, $target) }
        $staged = $null
    } catch {
        throw "Could not replace govm.exe. Close running govm processes and check directory permissions, then retry. $($_.Exception.Message)"
    }
    function Add-GovmPath([string] $value) {
        $parts = New-Object 'System.Collections.Generic.List[string]'
        $seen = New-Object 'System.Collections.Generic.HashSet[string]' ([StringComparer]::OrdinalIgnoreCase)
        foreach ($entry in (@($installDir) + @($value -split ';'))) {
            if ([string]::IsNullOrWhiteSpace($entry)) { continue }
            $key = [Environment]::ExpandEnvironmentVariables($entry.Trim().Trim('"')).TrimEnd('\', '/')
            if ($seen.Add($key)) { $parts.Add($entry) }
        }
        return $parts -join ';'
    }
    $userPath = [Environment]::GetEnvironmentVariable('Path', 'User')
    [Environment]::SetEnvironmentVariable('Path', (Add-GovmPath $userPath), 'User')
    $env:Path = Add-GovmPath $env:Path
    Write-Host "Installed $target ($version). PATH is active in this session and new terminals."
    Write-Host 'Verify with: govm version'
    Write-Host 'Install Go separately with: govm install latest'
} catch {
    throw "govm installation failed: $($_.Exception.Message)"
} finally {
    [Net.ServicePointManager]::SecurityProtocol = $oldProtocol
    if ($staged -and (Test-Path -LiteralPath $staged)) { Remove-Item -LiteralPath $staged -Force }
    if ($tempDir -and (Test-Path -LiteralPath $tempDir)) { Remove-Item -LiteralPath $tempDir -Recurse -Force }
}
