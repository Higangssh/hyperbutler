# bench/compose: Docker Compose end to end on hyperbutler, through the bundled docker CLI
# and compose plugin.
#
#   powershell -File bench\compose\run.ps1 -Docker docker.exe [-Port 18090]
#
# hyperbutler must be running (it uses the docker context of the current DOCKER_CONFIG;
# point it at a test install). It copies bench\compose\project to a new temporary folder
# and, with that project (a redis service with a named volume and a healthcheck, and an
# app built from a Dockerfile that reaches redis by its service name, reads a Windows
# folder through a bind mount and publishes a port):
#
#  1. `compose up -d --build --wait`: the image builds with a build argument from .env,
#     and both services are up, the app only after the db is healthy;
#  2. `compose ps` lists both services running;
#  3. GET http://localhost:<port>/ returns the build argument and the environment value
#     from .env, a counter kept in redis, and the content of a file in the bind mount;
#  4. a second GET increments the counter; a change to the Windows file shows at once;
#  5. `compose exec app redis-cli -h db ping` answers PONG (service-name DNS);
#  6. `compose logs db` has redis's ready line;
#  7. `compose down` then `compose up -d --wait`: the counter goes on (the named volume
#     kept the data);
#  8. `compose down -v`: the containers, network and volume are gone.
#
# It prints PASS/FAIL per check and exits 1 if any failed.

[CmdletBinding()]
param(
    [Parameter(Mandatory)][string]$Docker,
    [int]$Port = 18090
)

$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest
$script:failed = 0

# Run a program and capture its exit code, stdout and stderr (without PowerShell's
# stderr wrapping).
function Invoke-Tool([string]$exe, [string[]]$argv, [string]$cwd) {
    $psi = New-Object System.Diagnostics.ProcessStartInfo
    $psi.FileName = $exe
    $psi.Arguments = ($argv | ForEach-Object { if ($_ -match '[\s"]') { '"' + ($_ -replace '"', '\"') + '"' } else { $_ } }) -join ' '
    $psi.WorkingDirectory = $cwd
    $psi.UseShellExecute = $false
    $psi.RedirectStandardOutput = $true
    $psi.RedirectStandardError = $true
    $p = [System.Diagnostics.Process]::Start($psi)
    $out = $p.StandardOutput.ReadToEndAsync()
    $err = $p.StandardError.ReadToEndAsync()
    $p.WaitForExit()
    [pscustomobject]@{ Code = $p.ExitCode; Out = $out.Result; Err = $err.Result }
}

function Check([string]$name, [bool]$ok, [string]$detail = '') {
    if ($ok) { Write-Host "PASS  $name" } else { $script:failed++; Write-Host "FAIL  $name $detail" }
}

function Compose([string[]]$argv) {
    Invoke-Tool $Docker (@('compose') + $argv) $script:proj
}

function Get-Page {
    try {
        (Invoke-WebRequest -UseBasicParsing -TimeoutSec 10 "http://localhost:$Port/cgi-bin/index").Content
    } catch { "error: $_" }
}

function Field([string]$page, [string]$key) {
    if ($page -match "(?m)^$key=(.*)$") { $Matches[1].Trim() } else { '' }
}

$script:proj = Join-Path ([IO.Path]::GetTempPath()) ("hbcompose-" + [guid]::NewGuid().ToString('N').Substring(0, 8))
Copy-Item -Recurse (Join-Path $PSScriptRoot 'project') $script:proj
New-Item -ItemType Directory (Join-Path $script:proj 'shared') | Out-Null
$token = [guid]::NewGuid().ToString('N')
[IO.File]::WriteAllText((Join-Path $script:proj 'shared\host.txt'), $token)
# The shell script must keep LF line endings whatever git did on checkout.
$sh = Join-Path $script:proj 'app\serve.sh'
[IO.File]::WriteAllText($sh, ([IO.File]::ReadAllText($sh) -replace "`r`n", "`n"))
$env:APP_PORT = "$Port"

try {
    $r = Compose @('up', '-d', '--build', '--wait')
    Check 'up --build --wait' ($r.Code -eq 0) $r.Err

    $r = Compose @('ps', '--format', '{{.Service}} {{.State}}')
    $running = @($r.Out -split "`n" | Where-Object { $_ -match ' running' })
    Check 'ps: db and app running' ($running.Count -eq 2) $r.Out

    $p1 = Get-Page
    Check 'build argument from .env' ((Field $p1 'greeting') -eq 'hello-from-build') $p1
    Check 'environment from .env' ((Field $p1 'message') -eq 'hello-from-env') $p1
    Check 'bind mount from Windows' ((Field $p1 'shared') -eq $token) $p1
    $h1 = Field $p1 'hits'
    Check 'redis reached by service name' ($h1 -match '^\d+$') $p1

    $token2 = [guid]::NewGuid().ToString('N')
    [IO.File]::WriteAllText((Join-Path $script:proj 'shared\host.txt'), $token2)
    $p2 = Get-Page
    Check 'counter increments' ([int](Field $p2 'hits') -eq [int]$h1 + 1) $p2
    Check 'a change to the Windows file shows' ((Field $p2 'shared') -eq $token2) $p2

    $r = Compose @('exec', '-T', 'app', 'redis-cli', '-h', 'db', 'ping')
    Check 'exec: redis-cli -h db ping' ($r.Out.Trim() -eq 'PONG') "$($r.Out) $($r.Err)"

    $r = Compose @('logs', 'db')
    Check 'logs db' ($r.Out -match 'Ready to accept connections') $r.Err

    $r = Compose @('down')
    Check 'down' ($r.Code -eq 0) $r.Err
    $r = Compose @('up', '-d', '--wait')
    Check 'up again' ($r.Code -eq 0) $r.Err
    $p3 = Get-Page
    Check 'the named volume kept the data' ([int](Field $p3 'hits') -eq [int]$h1 + 2) $p3
} finally {
    $r = Compose @('down', '-v', '--rmi', 'local')
    Check 'down -v' ($r.Code -eq 0) $r.Err
    $v = Invoke-Tool $Docker @('volume', 'ls', '-q', '--filter', 'name=hbcompose_') $script:proj
    Check 'volume removed' ([string]::IsNullOrWhiteSpace($v.Out)) $v.Out
    Remove-Item -Recurse -Force $script:proj -ErrorAction SilentlyContinue
}

if ($script:failed -gt 0) { Write-Host "$($script:failed) check(s) failed"; exit 1 }
Write-Host 'all checks passed'
