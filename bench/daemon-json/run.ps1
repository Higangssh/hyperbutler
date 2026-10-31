# bench/daemon-json: REQ-UX-007 end to end, through the hyperbutler CLI.
#
#   powershell -File bench\daemon-json\run.ps1 -Hyperbutler hyperbutler.exe -Docker docker.exe
#       [-ImageDir DIR] [-Registry HOST:PORT]
#
# hyperbutler must be running (it uses the engine and the docker context of the current
# HYPERBUTLER_HOME and DOCKER_CONFIG; point both at a test install). It
#
#  1. runs registry:2 (plain HTTP) with -p 5000:5000 and --restart=always;
#  2. without a daemon.json, checks that a push to the registry's non-loopback address
#     fails (dockerd wants HTTPS);
#  3. writes daemon.json with "insecure-registries" and runs `config daemon apply`, then
#     checks that dockerd restarted but the VM did not (same VM id, same guest boot id),
#     that push and pull work, and that another container sees the image in the catalog;
#  4. checks that a key hyperbutler owns, invalid JSON and a key dockerd does not know
#     are refused, and leave the previous daemon.json in effect;
#  5. restarts hyperbutler with the invalid file in place and checks that the previous
#     daemon.json is still in effect after the VM restart;
#  6. removes daemon.json, applies, and removes the registry.
#
# -Registry is the address dockerd uses for the registry; the default is the guest's own
# address on the user-mode network (192.168.127.2), which is not loopback, so dockerd
# only talks plain HTTP to it when it is in insecure-registries. A LAN registry on another
# machine is tested the same way with -Registry and without step 1.
#
# It prints PASS/FAIL per check and exits 1 if any failed.

[CmdletBinding()]
param(
    [Parameter(Mandatory)][string]$Hyperbutler,
    [Parameter(Mandatory)][string]$Docker,
    [string]$ImageDir = '',
    [string]$Registry = '192.168.127.2:5000'
)

$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest
$script:failed = 0

# Run a program and capture its exit code, stdout and stderr (without PowerShell's
# stderr wrapping).
function Invoke-Tool([string]$exe, [string[]]$argv) {
    $psi = New-Object System.Diagnostics.ProcessStartInfo
    $psi.FileName = $exe
    $psi.Arguments = ($argv | ForEach-Object { if ($_ -match '[\s"]') { '"' + ($_ -replace '"', '\"') + '"' } else { $_ } }) -join ' '
    $psi.UseShellExecute = $false
    $psi.RedirectStandardOutput = $true
    $psi.RedirectStandardError = $true
    $p = [System.Diagnostics.Process]::Start($psi)
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

$paths = (hb @('config', 'path', '--json')).Out | ConvertFrom-Json
$daemonJson = $paths.daemonJson
function Write-Daemon([string]$text) {
    # PowerShell turns $null into '' for a [string] parameter: '' means "no file".
    if ([string]::IsNullOrEmpty($text)) { Remove-Item -Force -ErrorAction SilentlyContinue $daemonJson; return }
    [IO.File]::WriteAllText($daemonJson, $text, (New-Object System.Text.UTF8Encoding($false)))
}
function Status { (hb @('status', '--json')).Out | ConvertFrom-Json }
function BootId { (dk @('run', '--rm', 'busybox:1.37', 'cat', '/proc/sys/kernel/random/boot_id')).Out.Trim() }
function Insecure { (dk @('info', '--format', '{{json .RegistryConfig.IndexConfigs}}')).Out }

$st = Status
if (-not $st.running) { throw 'hyperbutler is not running; start it first' }
Write-Output "hyperbutler: engine pid $($st.state.pid), VM $($st.state.vmId), daemon.json $daemonJson"

# 0. A clean start: no daemon.json.
Write-Daemon $null
$r = hb @('config', 'daemon', 'apply')
Check 'apply without a daemon.json' ($r.Code -eq 0) (Show $r)

# 1. The registry.
$null = dk @('rm', '-f', 'hb-registry')
$r = dk @('run', '-d', '--restart=always', '-p', '5000:5000', '--name', 'hb-registry', 'registry:2')
Check 'registry:2 runs with -p 5000:5000' ($r.Code -eq 0) (Show $r)
$r = dk @('pull', 'busybox:1.37')
Check 'pull busybox' ($r.Code -eq 0) (Show $r)
$img = "$Registry/hb/busybox:1"
$null = dk @('tag', 'busybox:1.37', $img)
Start-Sleep -Seconds 2
try {
    $cat = Invoke-WebRequest -UseBasicParsing -TimeoutSec 5 http://127.0.0.1:5000/v2/_catalog
    Check 'registry answers on the host at 127.0.0.1:5000 (auto-publish)' ($cat.StatusCode -eq 200)
} catch { Check 'registry answers on the host at 127.0.0.1:5000 (auto-publish)' $false $_ }

# 2. Without insecure-registries, dockerd insists on HTTPS.
$r = dk @('push', $img)
Check "push to $Registry fails without insecure-registries" ($r.Code -ne 0 -and ($r.Err -match 'HTTPS|https')) (Show $r)

# 3. Apply insecure-registries: dockerd restarts, the VM does not.
$boot0 = BootId
$vm0 = (Status).state.vmId
$cfg = "{`r`n  `"insecure-registries`": [`"$Registry`"]`r`n}`r`n"
Write-Daemon $cfg
$t0 = Get-Date
$r = hb @('config', 'daemon', 'apply')
$took = ((Get-Date) - $t0).TotalSeconds
Check 'config daemon apply' ($r.Code -eq 0 -and $r.Out -match 'dockerd restarted') (Show $r)
Write-Output ("     apply took {0:N1} s" -f $took)
$st = Status
$sha = (Get-FileHash -Algorithm SHA256 $daemonJson).Hash.ToLower()
Check 'state: the file in effect is the user file, byte for byte (sha256)' ($st.state.daemonConfig.source -eq 'file' -and $st.state.daemonConfig.sha256 -eq $sha -and $st.state.daemonConfig.restarted) ($st.state.daemonConfig | ConvertTo-Json -Compress)
Check 'the VM kept running (same VM id)' ($st.state.vmId -eq $vm0) "$vm0 -> $($st.state.vmId)"
# Byte for byte in the guest: the installed file, read back through a container.
$r = dk @('run', '--rm', '-v', '/run/hyperbutler/daemon.json:/d:ro', 'busybox:1.37', 'sha256sum', '/d')
Check 'the guest file is the user file, byte for byte (sha256 in the guest)' ($r.Code -eq 0 -and $r.Out -match "^$sha ") (Show $r)
$boot1 = BootId
Check 'the guest did not reboot (same boot id)' ($boot1 -eq $boot0 -and $boot0 -ne '') "$boot0 -> $boot1"
Check 'dockerd lists the insecure registry' ((Insecure) -match [regex]::Escape($Registry)) (Insecure)
# --restart=always brings the registry back after the dockerd restart.
$up = $false
for ($i = 0; $i -lt 50 -and -not $up; $i++) {
    $up = (dk @('inspect', '-f', '{{.State.Running}}', 'hb-registry')).Out.Trim() -eq 'true'
    if (-not $up) { Start-Sleep -Milliseconds 200 }
}
Check 'the registry container is running again' $up
Start-Sleep -Seconds 1
$r = dk @('push', $img)
Check "push to $Registry over HTTP" ($r.Code -eq 0) (Show $r)
$null = dk @('rmi', $img)
$r = dk @('pull', $img)
Check "pull from $Registry over HTTP" ($r.Code -eq 0) (Show $r)
$r = dk @('run', '--rm', 'busybox:1.37', 'wget', '-qO-', "http://$Registry/v2/_catalog")
Check 'another container sees hb/busybox in the catalog' ($r.Code -eq 0 -and $r.Out -match 'hb/busybox') (Show $r)

# 4. Refusals leave the previous file in effect.
function Expect-Refused([string]$name, [string]$text, [string]$want) {
    Write-Daemon $text
    $r = hb @('config', 'daemon', 'apply')
    Check "$name is refused" ($r.Code -ne 0 -and ($r.Err -match $want)) (Show $r)
    $s = Status
    Check "$name leaves the previous daemon.json in effect" ($s.state.daemonConfig.sha256 -eq $sha -and ((Insecure) -match [regex]::Escape($Registry))) ($s.state.daemonConfig | ConvertTo-Json -Compress)
    Check "$name does not rewrite the user's file" ([IO.File]::ReadAllText($daemonJson) -eq $text)
}
Expect-Refused 'an owned key (hosts)' '{"hosts":["tcp://0.0.0.0:2375"]}' 'hyperbutler owns'
Expect-Refused 'an owned key in another case (Data-Root)' '{"Data-Root":"/tmp"}' 'hyperbutler owns'
Expect-Refused 'invalid JSON' '{"insecure-registries": [' 'not valid JSON|middle'
Expect-Refused 'a key dockerd does not know' '{"no-such-option": true}' 'dockerd refused'
Expect-Refused 'a file dockerd does not start with (rolled back)' '{"storage-driver": "zfs"}' 'did not start'
Expect-Refused 'an owned key (live-restore)' '{"live-restore": true}' 'hyperbutler owns'
$boot2 = BootId
Check 'the refusals did not reboot the guest' ($boot2 -eq $boot0) "$boot0 -> $boot2"

# 5. The previous file survives a VM restart while the user's file is invalid.
Write-Daemon '{"insecure-registries": ['
$ra = @('restart')
if ($ImageDir) { $ra += '--image-dir', $ImageDir }
$r = hb $ra
Check 'hyperbutler restart with an invalid daemon.json' ($r.Code -eq 0) (Show $r)
$s = Status
Check 'after the restart the previous daemon.json is in effect' ($s.state.daemonConfig.source -eq 'previous' -and $s.state.daemonConfig.sha256 -eq $sha -and $s.state.daemonConfig.error -ne '') ($s.state.daemonConfig | ConvertTo-Json -Compress)
Check 'and dockerd still lists the insecure registry' ((Insecure) -match [regex]::Escape($Registry)) (Insecure)

# 6. Clean up.
Write-Daemon $null
$r = hb @('config', 'daemon', 'apply')
Check 'removing daemon.json and applying' ($r.Code -eq 0 -and ((Status).state.daemonConfig.source -eq 'none')) (Show $r)
Check 'dockerd no longer lists the registry' (-not ((Insecure) -match [regex]::Escape($Registry))) (Insecure)
$null = dk @('rm', '-f', 'hb-registry')
$null = dk @('rmi', $img)

if ($script:failed -gt 0) { Write-Output "$script:failed check(s) failed"; exit 1 }
Write-Output 'all checks passed'
