# Boots the ADR 0005 prototype under QEMU-WHPX (non-elevated) and measures the time from
# starting qemu-system-x86_64.exe until the Docker API answers GET /_ping with "OK".
#
#   powershell -ExecutionPolicy Bypass -File guest\prototype\run-qemu.ps1 [-Runs 5] [-MemoryMB 512] [-Smoke] [-IdleSeconds 0]
#
# The API is reached through QEMU user networking (hostfwd to the guest's tcp:2375, where
# sockfwd relays to dockerd's Unix socket). This is a measurement harness only; winbox
# itself will use hvsock or a named pipe.
#   -Smoke        after the first ready run, pull and run hello-world through the API.
#   -IdleSeconds  after ready, wait this long and report QEMU's private bytes and
#                 working set (one run only is enough).
#   -NoFreePageReporting  attach virtio-balloon without free page reporting.
#   -GapSeconds   pause between runs.
# Columns: ready_ms is host wall time to a good /_ping. serial_ms is when the serial log
# file first became non-empty (QEMU start, firmware, kernel load, and early boot up to
# the console being enabled). guest_*_s are guest uptimes printed by init. QEMU's stderr goes to out\qemu-stderr-N.log,
# the serial console to out\serial-N.log.

[CmdletBinding()]
param(
    [int]$Runs = 5,
    [int]$MemoryMB = 512,
    [int]$Cpus = 2,
    [int]$Port = 23750,
    [switch]$Smoke,
    [int]$IdleSeconds = 0,
    [switch]$NoFreePageReporting,
    [int]$GapSeconds = 0,
    [string]$Qemu = 'C:\Program Files\qemu\qemu-system-x86_64.exe',
    [string]$Out = '',
    [string]$Initrd = 'initramfs-winbox.gz',
    [string]$Append = ''
)

$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest
$ProgressPreference = 'SilentlyContinue'
$proto = Split-Path -Parent $MyInvocation.MyCommand.Path
if (-not $Out) { $Out = Join-Path $proto '..\out' }
$Out = (Resolve-Path $Out).Path
$kernel = Join-Path $Out 'vmlinuz-virt'
$initrdPath = Join-Path $Out $Initrd
$balloon = if ($NoFreePageReporting) { 'virtio-balloon-pci' } else { 'virtio-balloon-pci,free-page-reporting=on' }
$api = "http://127.0.0.1:$Port"

function Invoke-Api([string]$method, [string]$path, [string]$body = $null, [int]$timeoutSec = 2) {
    $p = @{ UseBasicParsing = $true; Method = $method; Uri = "$api$path"; TimeoutSec = $timeoutSec }
    if ($body) { $p.Body = $body; $p.ContentType = 'application/json' }
    Invoke-WebRequest @p
}

# Poll with a fresh connection and a short read timeout each time. QEMU's slirp accepts
# the TCP connection before anything listens in the guest, and a pooled HTTP client can
# sit on a stale connection, so either would hide the real ready time.
function Test-Ping {
    $c = [Net.Sockets.TcpClient]::new()
    try {
        $c.NoDelay = $true
        $c.Connect('127.0.0.1', $Port)
        $s = $c.GetStream()
        $s.ReadTimeout = 250
        $req = [Text.Encoding]::ASCII.GetBytes("GET /_ping HTTP/1.0`r`n`r`n")
        $s.Write($req, 0, $req.Length)
        $buf = New-Object byte[] 1024
        $resp = ''
        while (($n = $s.Read($buf, 0, $buf.Length)) -gt 0) { $resp += [Text.Encoding]::ASCII.GetString($buf, 0, $n) }
        return $resp -match '^HTTP/1\.[01] 200' -and $resp.EndsWith('OK')
    } catch { return $false } finally { $c.Dispose() }
}

function Assert-PortFree {
    # A leftover VM on the same port would answer /_ping and fake a result.
    $l = [Net.Sockets.TcpListener]::new([Net.IPAddress]::Loopback, $Port)
    try { $l.Start() } catch { throw "127.0.0.1:$Port is already in use; is another VM still running?" } finally { $l.Stop() }
}

function Measure-Run([int]$run) {
    $log = Join-Path $Out "serial-$run.log"
    Remove-Item -Force -ErrorAction SilentlyContinue $log
    $qargs = @(
        '-accel', 'whpx', '-m', "$MemoryMB", '-smp', "$Cpus",
        '-display', 'none', '-monitor', 'none', '-serial', "file:$log",
        '-kernel', "`"$kernel`"", '-initrd', "`"$initrdPath`"",
        '-append', "`"console=ttyS0 panic=-1 $Append`"",
        '-netdev', "user,id=n0,hostfwd=tcp:127.0.0.1:$Port-10.0.2.15:2375",
        '-device', 'virtio-net-pci,netdev=n0',
        '-device', $balloon,
        '-device', 'virtio-rng-pci',
        '-no-reboot'
    )
    # Process.Start rather than Start-Process: with Start-Process, every launch after the
    # first one in the same PowerShell session took ~0.7 s longer to reach the first
    # guest console byte (measured 2026-10-30; cause not investigated).
    $psi = [Diagnostics.ProcessStartInfo]::new($Qemu, ($qargs -join ' '))
    $psi.UseShellExecute = $false
    $psi.CreateNoWindow = $true
    $psi.RedirectStandardError = $true
    $sw = [Diagnostics.Stopwatch]::StartNew()
    $proc = [Diagnostics.Process]::Start($psi)
    $stderrTask = $proc.StandardError.ReadToEndAsync()
    try {
        $ready = $null
        $serial = $null
        while ($sw.Elapsed.TotalSeconds -lt 60 -and -not $proc.HasExited) {
            if ($null -eq $serial -and (Test-Path $log) -and (Get-Item $log).Length -gt 0) {
                $serial = $sw.Elapsed.TotalMilliseconds
            }
            if (Test-Ping) { $ready = $sw.Elapsed.TotalMilliseconds; break }
            Start-Sleep -Milliseconds 10
        }
        if ($null -eq $ready -or $proc.HasExited) {
            Get-Content $log -ErrorAction SilentlyContinue | Select-Object -Last 40
            throw "run ${run}: Docker API not ready after 60 s (qemu exited: $($proc.HasExited))"
        }
        $info = (Invoke-Api GET '/info').Content | ConvertFrom-Json
        Start-Sleep -Milliseconds 300   # let the serial log flush
        $marks = @{}
        foreach ($l in Get-Content $log) {
            if ($l -match '^winbox-t (\S+) (\S+)') { $marks[$Matches[2]] = [double]$Matches[1] }
        }
        $row = [ordered]@{
            run = $run; ready_ms = [math]::Round($ready); serial_ms = [math]::Round($serial)
            guest_init_s = $marks['init-start']; guest_modules_s = $marks['modules-done']
            guest_dockerd_exec_s = $marks['dockerd-exec']; guest_sock_s = $marks['docker-sock']
            version = $info.ServerVersion; driver = $info.Driver; cgroup = "$($info.CgroupDriver)/v$($info.CgroupVersion)"
        }
        if ($Smoke -and $run -eq 1) {
            $t0 = $sw.Elapsed.TotalSeconds
            Invoke-Api POST '/images/create?fromImage=hello-world&tag=latest' -timeoutSec 120 | Out-Null
            $c = (Invoke-Api POST '/containers/create' '{"Image":"hello-world"}').Content | ConvertFrom-Json
            Invoke-Api POST "/containers/$($c.Id)/start" | Out-Null
            $w = (Invoke-Api POST "/containers/$($c.Id)/wait" -timeoutSec 30).Content | ConvertFrom-Json
            $logs = (Invoke-Api GET "/containers/$($c.Id)/logs?stdout=1").RawContentStream.ToArray()
            $text = [Text.Encoding]::UTF8.GetString($logs)
            $row.smoke = "exit=$($w.StatusCode) hello=$($text -match 'Hello from Docker') $([math]::Round($sw.Elapsed.TotalSeconds - $t0, 1))s"
        }
        if ($IdleSeconds -gt 0 -and $run -eq 1) {
            Start-Sleep -Seconds $IdleSeconds
            $proc.Refresh()
            $row.idle_private_mb = [math]::Round($proc.PrivateMemorySize64 / 1MB)
            $row.idle_ws_mb = [math]::Round($proc.WorkingSet64 / 1MB)
        }
        [pscustomobject]$row
    } finally {
        # Always stop the VM this run started, even when a step above throws.
        if (-not $proc.HasExited) { $proc.Kill() }
        $proc.WaitForExit()
        [IO.File]::WriteAllText((Join-Path $Out "qemu-stderr-$run.log"), $stderrTask.GetAwaiter().GetResult())
    }
}

$results = @()
for ($run = 1; $run -le $Runs; $run++) {
    if ($run -gt 1 -and $GapSeconds -gt 0) { Start-Sleep -Seconds $GapSeconds }
    Assert-PortFree
    $results += Measure-Run $run
}

$results | Format-Table -AutoSize | Out-String -Width 400
if ($Smoke -or $IdleSeconds -gt 0) { $results[0] | Format-List | Out-String -Width 400 }
$sorted = @($results | ForEach-Object { $_.ready_ms } | Sort-Object)
"median ready_ms: {0}  (n={1}, min {2}, max {3})" -f $sorted[[int][math]::Floor(($sorted.Count - 1) / 2)], $sorted.Count, $sorted[0], $sorted[-1]
