# bench/uninstall: REQ-UX-010 end to end, through the hyperbutler CLI.
#
#   powershell -File bench\uninstall\run.ps1 -Hyperbutler hyperbutler.exe -Docker docker.exe
#       -ImageDir DIR [-Root DIR] [-Pipe PIPE]
#
# Everything runs against a test installation under -Root (default
# %TEMP%\hb-dev\uninstall): its own HYPERBUTLER_HOME, data directory, Docker API pipe and
# DOCKER_CONFIG, so neither %LOCALAPPDATA%\hyperbutler nor the user's ~/.docker is touched.
# It
#
#  1. starts hyperbutler with the data disk in a separate directory that also holds a
#     file of the user's, runs a container and writes a volume;
#  2. checks that `uninstall --dry-run` lists the engine, the docker context, the data
#     disk, settings and logs, and that it changed nothing;
#  3. checks that `uninstall` without --yes and without a console refuses;
#  4. `uninstall --keep-data --yes` while running: the engine is stopped, the disk is
#     kept, a dry run with --keep-data lists nothing; a new start reuses the disk and the
#     volume is still there;
#  5. `uninstall --yes`: a dry run lists nothing, the docker context is gone and no longer
#     selected, the state directory is gone, the data directory and the user's file in it
#     stay, and no compute system of the installation is left.
#
# It prints PASS/FAIL per check and exits 1 if any failed.

[CmdletBinding()]
param(
    [Parameter(Mandatory)][string]$Hyperbutler,
    [Parameter(Mandatory)][string]$Docker,
    [Parameter(Mandatory)][string]$ImageDir,
    [string]$Root = (Join-Path $env:TEMP 'hb-dev\uninstall'),
    [string]$Pipe = '\\.\pipe\docker_engine_hb_uninst'
)

$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest
$script:failed = 0

$Root = [IO.Path]::GetFullPath($Root)
$home_ = Join-Path $Root 'home'
$data = Join-Path $Root 'data'
$dcfg = Join-Path $Root 'docker'
foreach ($p in @($home_, $dcfg)) {
    foreach ($real in @((Join-Path $env:LOCALAPPDATA 'hyperbutler'), (Join-Path $env:USERPROFILE '.docker'))) {
        if ($p.TrimEnd('\') -ieq $real) { throw "refusing to run against $real" }
    }
}
if (Test-Path $home_) { throw "$home_ exists; remove it (or pick another -Root) first" }
New-Item -ItemType Directory -Force $data, $dcfg | Out-Null
$sentinel = Join-Path $data 'keep-me.txt'
Set-Content -Path $sentinel -Value 'a file of the user''s'
$env:HYPERBUTLER_HOME = $home_
$env:DOCKER_CONFIG = $dcfg
Remove-Item Env:DOCKER_HOST, Env:DOCKER_CONTEXT -ErrorAction SilentlyContinue

# Run a program and capture its exit code, stdout and stderr (without PowerShell's
# stderr wrapping). Stdin is redirected, so the program does not see a console.
function Invoke-Tool([string]$exe, [string[]]$argv) {
    $psi = New-Object System.Diagnostics.ProcessStartInfo
    $psi.FileName = $exe
    $psi.Arguments = ($argv | ForEach-Object { if ($_ -match '[\s"]') { '"' + ($_ -replace '"', '\"') + '"' } else { $_ } }) -join ' '
    $psi.UseShellExecute = $false
    $psi.RedirectStandardInput = $true
    $psi.RedirectStandardOutput = $true
    $psi.RedirectStandardError = $true
    $p = [System.Diagnostics.Process]::Start($psi)
    $p.StandardInput.Close()
    $out = $p.StandardOutput.ReadToEndAsync()
    $err = $p.StandardError.ReadToEndAsync()
    $p.WaitForExit()
    [pscustomobject]@{ Code = $p.ExitCode; Out = $out.Result; Err = $err.Result }
}
function hb([string[]]$a) { Invoke-Tool $Hyperbutler $a }
function dk([string[]]$a) { Invoke-Tool $Docker $a }
function Check([string]$name, [bool]$ok, [string]$detail = '') {
    if ($ok) { Write-Output "PASS $name" } else { Write-Output "FAIL $name $detail"; $script:failed++ }
}
function Show($r) { "exit $($r.Code)`n$($r.Out)$($r.Err)".Trim() }
function DryRun([string[]]$extra = @()) {
    $r = hb (@('uninstall', '--dry-run', '--json') + $extra)
    if ($r.Code -ne 0) { throw "uninstall --dry-run: $(Show $r)" }
    $r.Out | ConvertFrom-Json
}
function Kinds($items) { @($items | ForEach-Object { $_.kind }) }
function Paths($items) { @($items | ForEach-Object { $_.path }) }
function CurrentContext {
    $cfg = Join-Path $dcfg 'config.json'
    if (-not (Test-Path $cfg)) { return '' }
    $j = Get-Content -Raw $cfg | ConvertFrom-Json
    if (@($j.PSObject.Properties | ForEach-Object { $_.Name }) -contains 'currentContext') { return $j.currentContext }
    ''
}
function Start-Hb([string[]]$extra = @()) {
    $r = hb (@('start', '--image-dir', $ImageDir, '--pipe', $Pipe, '--memory', '1GiB') + $extra)
    Check "hyperbutler start $extra" ($r.Code -eq 0) (Show $r)
}

try {
    # 1. A running installation with a container's data in it.
    Start-Hb @('--data', $data, '--disk-size', '8GiB')
    Check 'the docker context is selected' ((CurrentContext) -eq 'hyperbutler') (CurrentContext)
    $r = dk @('run', '--rm', '-v', 'hbvol:/v', 'busybox:1.37', 'sh', '-c', 'echo kept > /v/f')
    Check 'a container writes a volume' ($r.Code -eq 0) (Show $r)
    $status = (hb @('status', '--json')).Out | ConvertFrom-Json
    $disk = $status.disk

    # 2. The dry run lists everything and changes nothing.
    $plan = DryRun
    $k = Kinds $plan.items
    $p = Paths $plan.items
    Check 'dry run: the engine' ($k -contains 'engine') ($k -join ',')
    Check 'dry run: the docker context' ($k -contains 'docker-context') ($k -join ',')
    Check 'dry run: the data disk, settings, logs' (($p -contains $disk) -and ($p -contains (Join-Path $home_ 'settings.json')) -and ($p -contains (Join-Path $home_ 'logs\engine.log'))) ($p -join "`n")
    Check 'dry run: not the data directory that holds a file of the user''s' (-not ($p -contains $data)) ($p -join "`n")
    Check 'dry run: hyperbutler.exe is kept' ((Paths $plan.kept) -contains [IO.Path]::GetFullPath($Hyperbutler)) (Paths $plan.kept)
    Check 'the dry run changed nothing' ((hb @('status')).Code -eq 0 -and (Test-Path $disk) -and (CurrentContext) -eq 'hyperbutler')

    # 3. No console, no --yes: refused.
    $r = hb @('uninstall')
    Check 'uninstall without --yes and a console is refused' ($r.Code -ne 0 -and $r.Err -match '--yes') (Show $r)
    Check 'and hyperbutler still runs' ((hb @('status')).Code -eq 0)

    # 4. --keep-data while running.
    $r = hb @('uninstall', '--keep-data', '--yes', '--json')
    $res = $r.Out | ConvertFrom-Json
    Check 'uninstall --keep-data --yes' ($r.Code -eq 0 -and -not (@($res.PSObject.Properties | ForEach-Object { $_.Name }) -contains 'failed')) (Show $r)
    Check 'it stopped the engine' ((Kinds $res.items) -contains 'engine' -or (hb @('status')).Code -eq 3) (Show $r)
    Check 'the data disk is kept' ((Test-Path $disk) -and (Test-Path (Join-Path $data 'disk.json')))
    $after = DryRun @('--keep-data')
    Check 'a dry run with --keep-data lists nothing' (@($after.items).Count -eq 0) ($after.items | ConvertTo-Json -Compress)
    Check 'the context is gone after --keep-data' ((CurrentContext) -ne 'hyperbutler' -and -not (Test-Path (Join-Path $home_ 'settings.json')))
    Start-Hb @('--data', $data) # the disk's size is its own
    $r = dk @('run', '--rm', '-v', 'hbvol:/v', 'busybox:1.37', 'cat', '/v/f')
    Check 'a new start reuses the kept disk (the volume is there)' ($r.Code -eq 0 -and $r.Out.Trim() -eq 'kept') (Show $r)

    # 5. The full uninstall.
    $vmPrefix = ($status.state.vmId -replace '[0-9a-f]+$', '')
    $r = hb @('uninstall', '--yes', '--json')
    $res = $r.Out | ConvertFrom-Json
    Check 'uninstall --yes' ($r.Code -eq 0 -and -not (@($res.PSObject.Properties | ForEach-Object { $_.Name }) -contains 'failed')) (Show $r)
    $after = DryRun
    Check 'afterwards a dry run lists nothing' (@($after.items).Count -eq 0) ($after.items | ConvertTo-Json -Compress)
    Check 'the docker context is gone' (-not (Test-Path (Join-Path $dcfg 'contexts\meta\*\meta.json'))) (Get-ChildItem -Recurse $dcfg | Out-String)
    Check 'and no longer selected' ((CurrentContext) -ne 'hyperbutler') (CurrentContext)
    $r = dk @('context', 'ls', '--format', '{{.Name}}')
    Check 'docker context ls does not list it' ($r.Code -eq 0 -and -not ($r.Out -match '(?m)^hyperbutler$')) (Show $r)
    Check 'the state directory is gone' (-not (Test-Path $home_))
    Check 'the data disk is gone' (-not (Test-Path $disk) -and -not (Test-Path (Join-Path $data 'disk.json')))
    Check 'the data directory and the user''s file in it stay' ((Test-Path $sentinel) -and ((Paths $after.kept) -notcontains $data))
    Check "no compute system $vmPrefix* is left" (-not ((Kinds $after.items) -contains 'vm'))
    $r = hb @('status', '--json')
    Check 'status: not set up' ($r.Code -eq 3 -and ($r.Out | ConvertFrom-Json).setUp -eq $false) (Show $r)
} finally {
    # Leave nothing running, whatever failed.
    if (Test-Path $home_) { $null = hb @('stop') }
}

if ($script:failed -gt 0) { Write-Output "$script:failed check(s) failed"; exit 1 }
Write-Output 'all checks passed'
