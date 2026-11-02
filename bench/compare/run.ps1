# bench/compare: hyperbutler and Docker Desktop (WSL 2 backend) on the same machine, each
# with its out-of-the-box settings, measured the same way. See README.md.
#
#   powershell -File bench\compare\run.ps1 -Engine hyperbutler|desktop -Out DIR
#       [-Hyperbutler EXE] [-Home DIR] [-Docker EXE] [-Only start,idle,disk,reclaim,bind]
#       [-Runs 10] [-IdleSeconds 600]
#
# Each test writes a CSV to -Out and prints a summary line. Nothing here changes either
# engine's settings. hyperbutler runs from -Hyperbutler with its own -Home and talks to its
# own pipe; Docker Desktop is started and stopped with its CLI (`docker desktop`).

[CmdletBinding()]
param(
    [Parameter(Mandatory)] [ValidateSet('hyperbutler', 'desktop')] [string]$Engine,
    [Parameter(Mandatory)] [string]$Out,
    [string]$Hyperbutler = '',
    [string]$HbHome = '',
    [string]$Docker = 'C:\Program Files\Docker\Docker\resources\bin\docker.exe',
    [string[]]$Only = @('start', 'idle', 'disk', 'reclaim', 'bind'),
    [int]$Runs = 10,
    [int]$IdleSeconds = 600,
    [string[]]$Images = @('node:22', 'python:3.12', 'golang:1.24', 'rust:1.88')
)

$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest
# powershell -File passes "start,bind" as one string.
$Only = @($Only | ForEach-Object { $_ -split ',' } | Where-Object { $_ })
New-Item -ItemType Directory -Force $Out | Out-Null
$Out = (Resolve-Path $Out).Path

Add-Type @'
using System; using System.Runtime.InteropServices;
public static class CmpNative {
  [DllImport("kernel32.dll", CharSet = CharSet.Unicode, SetLastError = true)]
  static extern uint GetCompressedFileSizeW(string name, out uint high);
  // The bytes a file takes on the volume (sparse and compressed files take fewer than
  // their length).
  public static long Allocated(string path) {
    uint high; uint low = GetCompressedFileSizeW(path, out high);
    if (low == 0xFFFFFFFF && Marshal.GetLastWin32Error() != 0) return -1;
    return ((long)high << 32) | low;
  }
}
'@

function Note([string]$m) { Write-Host ("[{0:HH:mm:ss}] {1}" -f (Get-Date), $m) }
function Median([double[]]$v) { $s = @($v | Sort-Object); if (-not $s.Count) { return [double]::NaN }; $n = $s.Count; if ($n % 2) { $s[($n - 1) / 2] } else { ($s[$n / 2 - 1] + $s[$n / 2]) / 2 } }

# --- the two engines -----------------------------------------------------------------------
if ($Engine -eq 'hyperbutler') {
    if (-not $Hyperbutler -or -not $HbHome) { throw '-Hyperbutler and -HbHome are required for hyperbutler' }
    New-Item -ItemType Directory -Force $HbHome, (Join-Path $HbHome 'docker-config') | Out-Null
    $env:HYPERBUTLER_HOME = $HbHome
    $env:DOCKER_CONFIG = Join-Path $HbHome 'docker-config'
    $env:HYPERBUTLER_NO_LAUNCHER = '1'
    $dockerHost = 'npipe:////./pipe/docker_engine_hyperbutler'
} else {
    $dockerHost = 'npipe:////./pipe/dockerDesktopLinuxEngine'
    # Docker Desktop's docker config names its credential helper, which lives next to its
    # docker.exe; its installer puts that folder on PATH only for new sign-ins.
    $env:PATH = (Split-Path -Parent $Docker) + ';' + $env:PATH
}
# Windows PowerShell turns a native command's stderr into errors under -ErrorAction Stop;
# exit codes are checked instead.
function Dk { $ErrorActionPreference = 'Continue'; & $Docker -H $dockerHost @args }
function EngineUp { $ErrorActionPreference = 'Continue'; $null = Dk info --format '{{.ServerVersion}}' 2>&1; $LASTEXITCODE -eq 0 }
# Ready is a container that ran. `docker info` is not enough: Docker Desktop's backend
# answers it while its VM is still stopped (or paused by Resource Saver).
function Ready { $ErrorActionPreference = 'Continue'; $null = Dk run --rm alpine true 2>&1; $LASTEXITCODE -eq 0 }
function Sum($rows) { $s = 0; foreach ($r in @($rows | Where-Object { $_ })) { $s += [double]$r.WorkingSetPrivate }; $s }

# The processes that are the engine. hyperbutler: its engine process and its VM's vmmem and
# vmwp (the ones that appeared with it). Docker Desktop: everything it and its WSL 2 backend
# run, by name; vmcompute (the Host Compute Service both use) is left out of both.
$script:baseline = @{}
function Snapshot { $h = @{}; foreach ($p in Get-CimInstance Win32_Process -Property ProcessId, Name, ParentProcessId) { $h[[int]$p.ProcessId] = $p.Name }; $h }
function EnginePids {
    $now = Get-CimInstance Win32_Process -Property ProcessId, Name, ParentProcessId
    if ($Engine -eq 'hyperbutler') {
        $now | Where-Object { $_.Name -match '^(hyperbutler\.exe|vmmem|vmwp\.exe)$' -and -not $script:baseline.ContainsKey([int]$_.ProcessId) } | ForEach-Object { [int]$_.ProcessId }
    } else {
        $now | Where-Object { $_.Name -match '^(Docker Desktop\.exe|com\.docker\..*|docker-agent\.exe|vmmemWSL|vmmem|wsl\.exe|wslhost\.exe|wslrelay\.exe|wslservice\.exe)$' } | ForEach-Object { [int]$_.ProcessId }
    }
}
# Private working set (what Task Manager calls Memory), from the same performance counters
# for both engines.
function MemSample {
    $pids = @(EnginePids)
    $rows = Get-CimInstance Win32_PerfRawData_PerfProc_Process -Property IDProcess, Name, WorkingSetPrivate | Where-Object { $pids -contains [int]$_.IDProcess }
    [pscustomobject]@{
        Total = Sum $rows
        VM    = Sum ($rows | Where-Object { $_.Name -like 'vmmem*' })
        Count = @($rows).Count
        Names = (($rows | Group-Object { $_.Name -replace '#\d+$', '' } | ForEach-Object { "$($_.Name)x$($_.Count)" }) -join ' ')
    }
}
function VMGone {
    $n = if ($Engine -eq 'desktop') { 'vmmemWSL' } else { 'vmmem' }
    if ($Engine -eq 'desktop') { return -not (Get-Process -Name $n -ErrorAction SilentlyContinue) }
    -not (@(EnginePids) | ForEach-Object { Get-Process -Id $_ -ErrorAction SilentlyContinue } | Where-Object Name -EQ 'vmmem')
}
function StopEngine {
    $ErrorActionPreference = 'Continue'
    if ($Engine -eq 'hyperbutler') { & $Hyperbutler stop | Out-Null }
    else {
        Dk desktop stop 2>&1 | Out-Null
        wsl.exe --shutdown 2>&1 | Out-Null   # so every start boots the VM, as hyperbutler's does
    }
    for ($i = 0; $i -lt 120 -and -not (VMGone); $i++) { Start-Sleep -Milliseconds 500 }
    if (-not (VMGone)) { throw 'the VM did not go away after stop' }
}
# Start, and return the seconds until `docker info` answers and until a container ran.
function StartEngine {
    $ErrorActionPreference = 'Continue'
    $script:baseline = Snapshot
    $sw = [Diagnostics.Stopwatch]::StartNew()
    if ($Engine -eq 'hyperbutler') { & $Hyperbutler start *> $null }
    else { Dk desktop start --detach 2>&1 | Out-Null }
    while (-not (EngineUp)) {
        if ($sw.Elapsed.TotalSeconds -gt 300) { throw 'the engine did not answer within 300 s' }
        Start-Sleep -Milliseconds 100
    }
    $info = $sw.Elapsed.TotalSeconds
    while (-not (Ready)) {
        if ($sw.Elapsed.TotalSeconds -gt 300) { throw 'no container ran within 300 s' }
        Start-Sleep -Milliseconds 100
    }
    [pscustomobject]@{ Info = $info; Run = $sw.Elapsed.TotalSeconds }
}
# The alpine image the readiness check runs, pulled once.
function EnsureAlpine {
    if (-not (EngineUp)) { $null = StartEngine }
    $ErrorActionPreference = 'Continue'
    Dk pull -q alpine 2>&1 | Out-Null
}
function DataDisk {
    if ($Engine -eq 'hyperbutler') { Join-Path $HbHome 'data.vhdx' }
    else { Join-Path $env:LOCALAPPDATA 'Docker\wsl\disk\docker_data.vhdx' }
}
function DiskBytes { $f = Get-Item (DataDisk); [pscustomobject]@{ Length = $f.Length; Allocated = [CmpNative]::Allocated($f.FullName) } }

# --- tests -------------------------------------------------------------------------------------
$results = [ordered]@{ engine = $Engine; when = (Get-Date).ToString('s') }
# Every test starts from a stopped engine, so none inherits another's state.
function Fresh { if (EngineUp) { StopEngine }; $null = StartEngine }
EnsureAlpine

if ($Only -contains 'start') {
    Note "start: 1 warm-up + $Runs runs (stop, VM gone, start, time to docker info and to a container that ran)"
    if (EngineUp) { StopEngine }
    $times = @()
    for ($i = 0; $i -le $Runs; $i++) {
        $t = StartEngine
        if ($i -gt 0) { $times += $t; Note ("  run {0}: info {1:N2} s, container {2:N2} s" -f $i, $t.Info, $t.Run) } else { Note ("  warm-up: info {0:N2} s, container {1:N2} s" -f $t.Info, $t.Run) }
        StopEngine
    }
    $times | ForEach-Object { [pscustomobject]@{ engine = $Engine; infoSeconds = $_.Info; containerSeconds = $_.Run } } | Export-Csv -NoTypeInformation (Join-Path $Out "start-$Engine.csv")
    $run = @($times | ForEach-Object { $_.Run })
    $results.startInfoMedian = Median @($times | ForEach-Object { $_.Info })
    $results.startContainerMedian = Median $run
    $results.startContainerMin = ($run | Measure-Object -Minimum).Minimum
    $results.startContainerMax = ($run | Measure-Object -Maximum).Maximum
    Note ("start: to a container that ran, median {0:N2} s (min {1:N2}, max {2:N2}); docker info median {3:N2} s" -f $results.startContainerMedian, $results.startContainerMin, $results.startContainerMax, $results.startInfoMedian)
}

if ($Only -contains 'idle') {
    Note "idle: start, no containers, sample every 10 s for $IdleSeconds s"
    Fresh
    $t0 = Get-Date; $rows = @()
    while (((Get-Date) - $t0).TotalSeconds -le $IdleSeconds) {
        $s = MemSample
        $rows += [pscustomobject]@{ engine = $Engine; second = [int]((Get-Date) - $t0).TotalSeconds; totalMiB = [math]::Round($s.Total / 1MB, 1); vmMiB = [math]::Round($s.VM / 1MB, 1); processes = $s.Count; names = $s.Names }
        Start-Sleep -Seconds 10
    }
    $rows | Export-Csv -NoTypeInformation (Join-Path $Out "idle-$Engine.csv")
    $late = @($rows | Where-Object { $_.second -ge 300 })
    $results.idleMedianMiB_5to10min = Median ($late | ForEach-Object { $_.totalMiB })
    $results.idleVmMedianMiB_5to10min = Median ($late | ForEach-Object { $_.vmMiB })
    $results.idleMaxMiB = ($rows | Measure-Object totalMiB -Maximum).Maximum
    $results.idleProcesses = ($late | Select-Object -Last 1).processes
    $results.idleProcessNames = ($late | Select-Object -Last 1).names
    Note ("idle: median {0:N0} MiB from minute 5 to 10 (VM {1:N0} MiB), {2} processes: {3}" -f $results.idleMedianMiB_5to10min, $results.idleVmMedianMiB_5to10min, $results.idleProcesses, $results.idleProcessNames)
}

if ($Only -contains 'disk') {
    Note "disk: pull $($Images -join ', '), delete them, watch the data disk on the host"
    Fresh
    $rows = @()
    $mark = { param($phase) $d = DiskBytes; $script:rows += [pscustomobject]@{ engine = $Engine; phase = $phase; lengthGiB = [math]::Round($d.Length / 1GB, 2); allocatedGiB = [math]::Round($d.Allocated / 1GB, 2) }; Note ("  {0}: file {1:N2} GiB, on disk {2:N2} GiB" -f $phase, ($d.Length / 1GB), ($d.Allocated / 1GB)) }
    $script:rows = @()
    & $mark 'before'
    foreach ($img in $Images) { Dk pull -q $img | Out-Null; if ($LASTEXITCODE) { throw "pull $img failed" } }
    & $mark 'after pull'
    Dk rmi $Images | Out-Null
    Dk system prune -af | Out-Null
    & $mark 'after rmi+prune'
    Start-Sleep -Seconds 300
    & $mark 'after 5 min'
    StopEngine
    & $mark 'after stop'
    $script:rows | Export-Csv -NoTypeInformation (Join-Path $Out "disk-$Engine.csv")
    $results.disk = $script:rows
}

if ($Only -contains 'reclaim') {
    Note 'reclaim: a container fills 2 GiB of page cache and exits; VM memory before, right after, and 2 min later'
    Fresh
    Start-Sleep -Seconds 60
    $before = (MemSample).VM
    Dk run --rm alpine sh -c 'dd if=/dev/zero of=/tmp/fill bs=1M count=2048 status=none && cat /tmp/fill > /dev/null && rm /tmp/fill' | Out-Null
    $after = (MemSample).VM
    Start-Sleep -Seconds 120
    $later = (MemSample).VM
    $results.reclaimVmMiB = [ordered]@{ before = [math]::Round($before / 1MB); after = [math]::Round($after / 1MB); after2min = [math]::Round($later / 1MB) }
    [pscustomobject]$results.reclaimVmMiB | Export-Csv -NoTypeInformation (Join-Path $Out "reclaim-$Engine.csv")
    Note ("reclaim: VM {0} MiB before, {1} right after, {2} two minutes later" -f $results.reclaimVmMiB.before, $results.reclaimVmMiB.after, $results.reclaimVmMiB.after2min)
}

if ($Only -contains 'bind') {
    Note 'bind: a Windows folder mounted into a container: 2000 small files, one 512 MiB file'
    Fresh
    $src = Join-Path $Out "bind-$Engine"
    if (Test-Path $src) { Remove-Item -Recurse -Force $src }
    New-Item -ItemType Directory (Join-Path $src 'small') | Out-Null
    $buf = New-Object byte[] 4096; (New-Object Random 1).NextBytes($buf)
    for ($i = 0; $i -lt 2000; $i++) { [IO.File]::WriteAllBytes((Join-Path $src "small\f$i.txt"), $buf) }
    $fs = [IO.File]::Create((Join-Path $src 'big.bin')); $fs.SetLength(512MB); $fs.Close()
    $script = @'
t() { s=$(cut -d' ' -f1 /proc/uptime); "$@" >/dev/null 2>&1; e=$(cut -d' ' -f1 /proc/uptime); awk "BEGIN { print int(($e - $s) * 1000) }"; }
echo "stat2000 $(t sh -c 'find /w/small -type f -exec stat {} + ')"
echo "read2000 $(t sh -c 'cat /w/small/*')"
echo "readbig $(t cat /w/big.bin)"
echo "write2000 $(t sh -c 'mkdir -p /w/out && for i in $(seq 1 2000); do echo x > /w/out/f$i; done')"
echo "writebig $(t sh -c 'dd if=/dev/zero of=/w/outbig bs=1M count=512 conv=fsync status=none')"
rm -rf /w/out /w/outbig
'@
    # In a file: Windows PowerShell mangles quotes in arguments to native commands.
    [IO.File]::WriteAllText((Join-Path $src 't.sh'), $script.Replace("`r`n", "`n"))
    $rows = @()
    for ($r = 1; $r -le 3; $r++) {
        $o = Dk run --rm -v "${src}:/w" alpine sh /w/t.sh
        foreach ($l in $o) { $k, $v = -split $l; $rows += [pscustomobject]@{ engine = $Engine; run = $r; test = $k; ms = [int]$v } }
    }
    $rows | Export-Csv -NoTypeInformation (Join-Path $Out "bind-$Engine.csv")
    $results.bindMedianMs = [ordered]@{}
    foreach ($g in $rows | Group-Object test) { $results.bindMedianMs[$g.Name] = Median ($g.Group | ForEach-Object { $_.ms }) }
    Note ('bind: ' + (($results.bindMedianMs.GetEnumerator() | ForEach-Object { "$($_.Key) $($_.Value) ms" }) -join ', '))
    Remove-Item -Recurse -Force $src
    StopEngine
}

$results | ConvertTo-Json -Depth 5 | Set-Content -Encoding utf8 (Join-Path $Out "summary-$Engine.json")
Note "wrote $Out\summary-$Engine.json"
