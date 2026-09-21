param(
    [ValidateSet("scanner-a", "scanner-b", "scanner-c")]
    [string]$ScannerId = "scanner-a",
    [ValidatePattern("^[A-Za-z0-9._-]+$")]
    [string]$WorkerVersion = "2.0.0",
    [ValidatePattern("^[A-Za-z0-9._-]+$")]
    [string]$SignatureVersion = "2"
)

$ErrorActionPreference = "Stop"
$root = Split-Path -Parent $PSScriptRoot
$extension = if ($IsWindows -or $env:OS -eq "Windows_NT") { ".exe" } else { "" }
$artifactName = "$ScannerId$extension"
$releaseDirectory = Join-Path $PSScriptRoot "releases\$ScannerId\$WorkerVersion"
$artifactPath = Join-Path $releaseDirectory $artifactName
$manifestPath = Join-Path $releaseDirectory "manifest.json"

New-Item -ItemType Directory -Force -Path $releaseDirectory | Out-Null
Push-Location $root
try {
    & go build -o $artifactPath "./cmd/$ScannerId"
    if ($LASTEXITCODE -ne 0) {
        throw "build $ScannerId failed"
    }
}
finally {
    Pop-Location
}

$manifest = [ordered]@{
    scannerId = $ScannerId
    workerVersion = $WorkerVersion
    signatureVersion = $SignatureVersion
    artifactPath = $artifactName
    sha256 = (Get-FileHash -Algorithm SHA256 $artifactPath).Hash.ToLowerInvariant()
} | ConvertTo-Json

[System.IO.File]::WriteAllText($manifestPath, $manifest, [System.Text.UTF8Encoding]::new($false))
Write-Host "Published http://127.0.0.1:8080/releases/$ScannerId/$WorkerVersion/manifest.json"
