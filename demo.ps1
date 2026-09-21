$ErrorActionPreference = "Stop"
$root = $PSScriptRoot
$demo = Join-Path $root ".demo"
$bin = Join-Path $demo "bin"
$release = Join-Path $demo "releases\scanner-a"
New-Item -ItemType Directory -Force -Path $bin, $release | Out-Null

$exe = if ($IsWindows -or $env:OS -eq "Windows_NT") { ".exe" } else { "" }
$miniav = Join-Path $bin "miniav$exe"
$scannerA = Join-Path $bin "scanner-a$exe"
$scannerB = Join-Path $bin "scanner-b$exe"

& go build -o $miniav ./cmd/miniav
if ($LASTEXITCODE -ne 0) { throw "build miniav failed" }
& go build -o $scannerA ./cmd/scanner-a
if ($LASTEXITCODE -ne 0) { throw "build scanner-a failed" }
& go build -o $scannerB ./cmd/scanner-b
if ($LASTEXITCODE -ne 0) { throw "build scanner-b failed" }

$config = Join-Path $demo "miniav.json"
$configJson = @{
    startupTimeoutMs = 3000
    scanTimeoutMs = 1500
    workers = @(
        @{ scannerId = "scanner-a"; workerVersion = "1.0.0"; executable = $scannerA; signaturePath = (Join-Path $root "samples\signatures-a.txt"); delayMs = 500 },
        @{ scannerId = "scanner-b"; workerVersion = "1.0.0"; executable = $scannerB; signaturePath = (Join-Path $root "samples\signatures-b.txt") }
    )
} | ConvertTo-Json -Depth 4
[System.IO.File]::WriteAllText($config, $configJson, [System.Text.UTF8Encoding]::new($false))

$core = Start-Process -FilePath $miniav -ArgumentList @("serve", "--config", $config) -PassThru -WindowStyle Hidden
try {
    $ready = $false
    for ($attempt = 0; $attempt -lt 50; $attempt++) {
        $probe = [System.Net.Sockets.TcpClient]::new()
        try {
            $pending = $probe.BeginConnect("127.0.0.1", 7331, $null, $null)
            if ($pending.AsyncWaitHandle.WaitOne(100)) {
                $probe.EndConnect($pending)
                $ready = $true
                break
            }
        }
        catch {}
        finally { $probe.Dispose() }
        Start-Sleep -Milliseconds 100
    }
    if (-not $ready) { throw "Core did not become ready" }

    $initialPid = $core.Id
    Write-Host "Core PID: $initialPid"
    & $miniav scan (Join-Path $root "samples\clean.txt")
    & $miniav scan (Join-Path $root "samples\test_virus.txt")
    & $miniav reload scanner-a (Join-Path $root "samples\signatures-a-v2.txt")
    & $miniav scan (Join-Path $root "samples\test_virus.txt")

    $v2Dir = Join-Path $release "2.0.0"
    New-Item -ItemType Directory -Force -Path $v2Dir | Out-Null
    $v2Artifact = Join-Path $v2Dir "scanner-a$exe"
    Copy-Item -Force $scannerA $v2Artifact
    $v2Manifest = Join-Path $v2Dir "manifest.json"
    $v2Json = @{
        scannerId = "scanner-a"
        workerVersion = "2.0.0"
        signatureVersion = "2"
        artifactPath = $v2Artifact
        sha256 = (Get-FileHash -Algorithm SHA256 $v2Artifact).Hash.ToLowerInvariant()
    } | ConvertTo-Json
    [System.IO.File]::WriteAllText($v2Manifest, $v2Json, [System.Text.UTF8Encoding]::new($false))

    $scanJob = Start-Job -ScriptBlock { param($app, $sample) & $app scan $sample } -ArgumentList $miniav, (Join-Path $root "samples\clean.txt")
    Start-Sleep -Milliseconds 100
    & $miniav update --manifest $v2Manifest
    Receive-Job -Wait -AutoRemoveJob $scanJob
    & $miniav status

    $v3Dir = Join-Path $release "3.0.0"
    New-Item -ItemType Directory -Force -Path $v3Dir | Out-Null
    $v3Artifact = Join-Path $v3Dir "scanner-a-broken$exe"
    [System.IO.File]::WriteAllText($v3Artifact, "intentionally invalid worker artifact", [System.Text.UTF8Encoding]::new($false))
    $v3Manifest = Join-Path $v3Dir "manifest.json"
    $v3Json = @{
        scannerId = "scanner-a"
        workerVersion = "3.0.0"
        signatureVersion = "3"
        artifactPath = $v3Artifact
        sha256 = (Get-FileHash -Algorithm SHA256 $v3Artifact).Hash.ToLowerInvariant()
    } | ConvertTo-Json
    [System.IO.File]::WriteAllText($v3Manifest, $v3Json, [System.Text.UTF8Encoding]::new($false))
    & $miniav update --manifest $v3Manifest
    if ($LASTEXITCODE -eq 0) { throw "broken update unexpectedly succeeded" }
    & $miniav status

    if ($core.Id -ne $initialPid) { throw "Core PID changed" }
    & $miniav shutdown
    $core.WaitForExit()
    Write-Host "Demo completed with stable Core PID $initialPid"
}
finally {
    if (-not $core.HasExited) {
        Stop-Process -Id $core.Id -Force
    }
}
