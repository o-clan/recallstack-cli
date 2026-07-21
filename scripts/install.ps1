param(
    [switch]$Help
)

$ErrorActionPreference = "Stop"
$DefaultVersion = "1.2.2"
$Version = if ($env:RECALLSTACK_VERSION) { $env:RECALLSTACK_VERSION } else { $DefaultVersion }

if ($Help) {
    Write-Output "Install Recallstack CLI $Version"
    Write-Output "Set RECALLSTACK_VERSION, RECALLSTACK_INSTALL_DIR, RECALLSTACK_INSTALL_SOURCE_DIR, or RECALLSTACK_RELEASE_BASE_URL to override defaults."
    exit 0
}

$InstallDir = if ($env:RECALLSTACK_INSTALL_DIR) {
    $env:RECALLSTACK_INSTALL_DIR
} else {
    Join-Path $env:LOCALAPPDATA "Recallstack\bin"
}
$SourceDir = $env:RECALLSTACK_INSTALL_SOURCE_DIR
$BaseUrl = if ($env:RECALLSTACK_RELEASE_BASE_URL) {
    $env:RECALLSTACK_RELEASE_BASE_URL.TrimEnd("/")
} else {
    "https://github.com/o-clan/recallstack-cli/releases/download/v$Version"
}

$Architecture = [System.Runtime.InteropServices.RuntimeInformation]::OSArchitecture.ToString().ToLowerInvariant()
switch ($Architecture) {
    "x64" { $Arch = "amd64" }
    "arm64" { $Arch = "arm64" }
    default { throw "recallstack installer: unsupported Windows architecture $Architecture" }
}

$Artifact = "recallstack_${Version}_windows_${Arch}.exe"
$TemporaryDir = $null
$TemporaryDestination = $null
try {
    if ($SourceDir) {
        $ArtifactPath = Join-Path $SourceDir $Artifact
        $ChecksumPath = Join-Path $SourceDir "SHA256SUMS"
    } else {
        $TemporaryDir = Join-Path ([System.IO.Path]::GetTempPath()) ("recallstack-install-" + [guid]::NewGuid())
        New-Item -ItemType Directory -Path $TemporaryDir | Out-Null
        $ArtifactPath = Join-Path $TemporaryDir $Artifact
        $ChecksumPath = Join-Path $TemporaryDir "SHA256SUMS"
        Invoke-WebRequest -UseBasicParsing -Uri "$BaseUrl/$Artifact" -OutFile $ArtifactPath
        Invoke-WebRequest -UseBasicParsing -Uri "$BaseUrl/SHA256SUMS" -OutFile $ChecksumPath
    }

    if (-not (Test-Path -LiteralPath $ArtifactPath)) { throw "recallstack installer: missing $Artifact" }
    if (-not (Test-Path -LiteralPath $ChecksumPath)) { throw "recallstack installer: missing SHA256SUMS" }

    $EscapedArtifact = [regex]::Escape($Artifact)
    $ChecksumLine = Get-Content -LiteralPath $ChecksumPath | Where-Object { $_ -match "^([a-fA-F0-9]{64})\s+\*?${EscapedArtifact}$" } | Select-Object -First 1
    if (-not $ChecksumLine) { throw "recallstack installer: checksum missing for $Artifact" }
    $Expected = ([regex]::Match($ChecksumLine, "^[a-fA-F0-9]{64}")).Value.ToLowerInvariant()
    $Actual = (Get-FileHash -Algorithm SHA256 -LiteralPath $ArtifactPath).Hash.ToLowerInvariant()
    if ($Actual -ne $Expected) { throw "recallstack installer: checksum verification failed" }

    New-Item -ItemType Directory -Force -Path $InstallDir | Out-Null
    $Destination = Join-Path $InstallDir "recallstack.exe"
    $TemporaryDestination = Join-Path $InstallDir (".recallstack-install-" + [guid]::NewGuid() + ".exe")
    Copy-Item -LiteralPath $ArtifactPath -Destination $TemporaryDestination
    $InstalledVersion = (& $TemporaryDestination --version | Out-String).Trim()
    if ($InstalledVersion -notmatch [regex]::Escape($Version)) {
        throw "recallstack installer: installed version check failed: $InstalledVersion"
    }
    Move-Item -Force -LiteralPath $TemporaryDestination -Destination $Destination

    $UserPath = [Environment]::GetEnvironmentVariable("Path", "User")
    $PathEntries = if ($UserPath) { $UserPath.Split(";", [System.StringSplitOptions]::RemoveEmptyEntries) } else { @() }
    if ($PathEntries -notcontains $InstallDir) {
        $NextUserPath = (($PathEntries + $InstallDir) -join ";")
        [Environment]::SetEnvironmentVariable("Path", $NextUserPath, "User")
    }
    if (($env:Path.Split(";", [System.StringSplitOptions]::RemoveEmptyEntries)) -notcontains $InstallDir) {
        $env:Path = "$InstallDir;$env:Path"
    }

    Write-Output "Recallstack CLI $Version installed to $Destination"
    Write-Output "Version: $InstalledVersion"
    Write-Output "Run: recallstack --help"
} finally {
    if ($TemporaryDestination -and (Test-Path -LiteralPath $TemporaryDestination)) {
        Remove-Item -Force -LiteralPath $TemporaryDestination
    }
    if ($TemporaryDir -and (Test-Path -LiteralPath $TemporaryDir)) {
        Remove-Item -Recurse -Force -LiteralPath $TemporaryDir
    }
}
