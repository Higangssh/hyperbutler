# bench/compat-tools: real client tools against a running hyperbutler (REQ-API-002,
# REQ-API-003). See README.md.
#
#   powershell -File bench\compat-tools\run.ps1 -ImageDir DIR
#
# It builds hyperbutler from this checkout (or takes -Hyperbutler), starts an isolated
# engine on its own pipe, runs each tool's basic sample, stops the engine, and prints and
# writes one result per tool: pass, fail or skipped, with the reason, the tool's version
# and the setting it needed. It exits 1 if a tool failed.
#
# Everything lives under -Root (default %TEMP%\hb-dev\compat): the engine's home, the
# docker config, a home folder and caches for the tools. Nothing is installed globally
# and neither %LOCALAPPDATA%\hyperbutler nor ~/.docker is read or written.

[CmdletBinding()]
param(
    [Parameter(Mandatory)][string]$ImageDir,
    [string]$Hyperbutler = '',
    [string]$Docker = '',
    [string]$Root = (Join-Path ([IO.Path]::GetTempPath()) 'hb-dev\compat'),
    [string]$Pipe = 'hb_compat',
    [string]$Memory = '4GiB',
    [string]$DiskSize = '32GiB',
    [string]$Out = '',
    # Sample ids to run (default all): go-sdk go-tc py-sdk py-tc node-tc java-tc dotnet-tc
    # devcontainer-image devcontainer-features.
    [string[]]$Only = @(),
    # Without the runs that try each tool with a docker context and with
    # ~/.testcontainers.properties instead of DOCKER_HOST.
    [switch]$SkipSettings,
    # Without the run on \\.\pipe\docker_engine (the documented opt-in).
    [switch]$SkipOptIn
)

$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest

$repo = (Resolve-Path (Join-Path $PSScriptRoot '..\..')).Path
$Root = [IO.Path]::GetFullPath($Root)
foreach ($real in @((Join-Path $env:LOCALAPPDATA 'hyperbutler'), (Join-Path $env:USERPROFILE '.docker'))) {
    if ($Root -ieq $real -or $Root.StartsWith("$real\", [StringComparison]::OrdinalIgnoreCase)) {
        throw "-Root must not be $real"
    }
}
if (-not $Out) { $Out = Join-Path $repo ('bench\results\compat-tools\' + (Get-Date -Format 'yyyyMMdd-HHmmss')) }

$engineHome = Join-Path $Root 'engine-home'
$dockerConfig = Join-Path $Root 'docker-config'
$contextConfig = Join-Path $Root 'docker-config-context'
$toolHome = Join-Path $Root 'tool-home'
$cache = Join-Path $Root 'cache'
$work = Join-Path $Root 'work'
$tcProps = Join-Path $toolHome '.testcontainers.properties'
$logs = Join-Path $Out 'logs'
foreach ($d in @($engineHome, $dockerConfig, $contextConfig, $toolHome, "$toolHome\AppData\Roaming", "$toolHome\AppData\Local",
        $cache, "$Root\tmp", $work, $logs)) {
    New-Item -ItemType Directory -Force $d | Out-Null
}
$pipePath = "\\.\pipe\$Pipe"
$dockerHost = "npipe:////./pipe/$Pipe"
# Testcontainers for Node parses DOCKER_HOST as a URL, which drops the path's "." and leaves
# //pipe/NAME. A backslash after the dot keeps it; other tools do not take this spelling.
$altHost = "npipe:////.\pipe\$Pipe"

$script:results = New-Object System.Collections.ArrayList
$script:settingRows = New-Object System.Collections.ArrayList
$script:optIn = $null
$script:logSeq = 0

# --- running programs ------------------------------------------------------------------

# Run a program with extra environment ($null removes a variable), to a log file, with a
# time limit. On a timeout only the tree of this process is killed.
function Invoke-Tool([string]$exe, [string[]]$argv, [string]$cwd, [hashtable]$vars, [int]$timeoutSec, [string]$logName) {
    $psi = New-Object System.Diagnostics.ProcessStartInfo
    $psi.FileName = $exe
    $psi.Arguments = ($argv | ForEach-Object { if ($_ -eq '' -or $_ -match '[\s"]') { '"' + ($_ -replace '"', '\"') + '"' } else { $_ } }) -join ' '
    $psi.WorkingDirectory = $cwd
    $psi.UseShellExecute = $false
    $psi.RedirectStandardOutput = $true
    $psi.RedirectStandardError = $true
    foreach ($k in $vars.Keys) {
        if ($psi.EnvironmentVariables.ContainsKey($k)) { $psi.EnvironmentVariables.Remove($k) }
        if ($null -ne $vars[$k]) { $psi.EnvironmentVariables[$k] = [string]$vars[$k] }
    }
    $t0 = Get-Date
    $p = [System.Diagnostics.Process]::Start($psi)
    $so = $p.StandardOutput.ReadToEndAsync()
    $se = $p.StandardError.ReadToEndAsync()
    $timedOut = -not $p.WaitForExit($timeoutSec * 1000)
    if ($timedOut) {
        & "$env:SystemRoot\System32\taskkill.exe" /T /F /PID $p.Id 2>$null | Out-Null
        [void]$p.WaitForExit(10000)
    }
    [void]$so.Wait(5000)
    [void]$se.Wait(5000)
    $stdout = if ($so.IsCompleted) { $so.Result } else { '' }
    $stderr = if ($se.IsCompleted) { $se.Result } else { '' }
    $code = if ($timedOut) { -1 } else { $p.ExitCode }
    $secs = [math]::Round(((Get-Date) - $t0).TotalSeconds, 1)
    if ($logName) {
        $script:logSeq++
        $path = Join-Path $logs ('{0:D3}-{1}.log' -f $script:logSeq, $logName)
        $text = "> $exe $($psi.Arguments)`r`n(cwd $cwd; exit $code; $secs s; timed out: $timedOut)`r`n--- stdout`r`n$stdout`r`n--- stderr`r`n$stderr`r`n"
        [IO.File]::WriteAllText($path, $text)
    }
    [pscustomobject]@{ Code = $code; Out = $stdout; Err = $stderr; TimedOut = $timedOut; Seconds = $secs }
}

function EngineVars { @{ HYPERBUTLER_HOME = $engineHome; DOCKER_CONFIG = $dockerConfig; HYPERBUTLER_NO_LAUNCHER = '1'; DOCKER_HOST = $null; DOCKER_CONTEXT = $null } }

function Hb([string[]]$argv, [int]$timeoutSec = 300, [string]$logName = 'hyperbutler') {
    Invoke-Tool $Hyperbutler $argv $Root (EngineVars) $timeoutSec $logName
}

# The environment of a tool under test. Its home, caches and temp folder are under -Root,
# which also keeps a real ~/.testcontainers.properties and ~/.docker out of the result.
function ToolVars([string]$mode) {
    $v = @{
        DOCKER_CONFIG = $dockerConfig; DOCKER_HOST = $null; DOCKER_CONTEXT = $null; DOCKER_TLS_VERIFY = $null; DOCKER_CERT_PATH = $null
        TESTCONTAINERS_RYUK_DISABLED = $null; TESTCONTAINERS_HOST_OVERRIDE = $null; TESTCONTAINERS_DOCKER_SOCKET_OVERRIDE = $null; COMPAT_LEAK = $null
        USERPROFILE = $toolHome; HOME = $toolHome; APPDATA = "$toolHome\AppData\Roaming"; LOCALAPPDATA = "$toolHome\AppData\Local"
        TEMP = "$Root\tmp"; TMP = "$Root\tmp"
        GOPATH = "$cache\gopath"; GOMODCACHE = "$cache\gopath\pkg\mod"; GOCACHE = "$cache\gobuild"; GOTELEMETRY = 'off'; GOFLAGS = $null
        PIP_CACHE_DIR = "$cache\pip"; PIP_DISABLE_PIP_VERSION_CHECK = '1'; PYTHONUTF8 = '1'
        npm_config_cache = "$cache\npm"; npm_config_update_notifier = 'false'; npm_config_fund = 'false'; npm_config_audit = 'false'
        NUGET_PACKAGES = "$cache\nuget"; DOTNET_CLI_HOME = $toolHome; DOTNET_CLI_TELEMETRY_OPTOUT = '1'; DOTNET_NOLOGO = '1'
        # Java takes its home from the system, not USERPROFILE.
        JAVA_TOOL_OPTIONS = "-Duser.home=$toolHome"
        PATH = (Split-Path $Docker) + ';' + $env:PATH
    }
    switch ($mode) {
        'docker-host' { $v.DOCKER_HOST = $dockerHost }
        'context' { $v.DOCKER_CONFIG = $contextConfig }
        'properties' { }
        'default' { }
        default { throw "unknown mode $mode" }
    }
    $v
}

function D([string[]]$argv) { Invoke-Tool $Docker $argv $Root (ToolVars 'docker-host') 120 '' }

# Remove every container of the isolated engine. Nothing else uses it.
function Clear-Containers {
    $r = D @('ps', '-aq')
    $ids = @($r.Out -split "\s+" | Where-Object { $_ })
    if ($ids.Count -gt 0) { [void](D (@('rm', '-f', '-v') + $ids)) }
}

# Up to 15 lines of what a tool said last, for the table; the log has it all.
function Get-Reason($r) {
    $lines = @((($r.Err + "`n" + $r.Out) -split "`r?`n") | ForEach-Object { $_.TrimEnd() } | Where-Object { $_ })
    $pick = @($lines | Where-Object { $_ -match '(?i)error|fail|exception|refused|denied|cannot|unable|assert|not found' })
    if ($pick.Count -eq 0) { $pick = $lines }
    $text = (($pick | Select-Object -Last 15) -join "`n")
    if ($r.TimedOut) { $text = "timed out after $($r.Seconds) s`n$text" }
    if ($text.Length -gt 2000) { $text = $text.Substring($text.Length - 2000) }
    $text
}

function Add-Result([string]$id, [string]$name, [string]$status, [string]$version, [string]$setting, [string]$note, [double]$seconds) {
    $row = [pscustomobject]@{ id = $id; tool = $name; status = $status; version = $version; setting = $setting; note = $note; seconds = $seconds }
    [void]$script:results.Add($row)
    $first = if ($note) { ($note -split "`n")[0] } else { '' }
    Write-Host ('{0,-8} {1} {2} {3}' -f $status.ToUpper(), $name, $version, $first)
}

function Find-Exe([string]$name) {
    $c = Get-Command $name -CommandType Application -ErrorAction SilentlyContinue | Select-Object -First 1
    if ($c) { $c.Source } else { $null }
}

function Want([string]$id) { $Only.Count -eq 0 -or $Only -contains $id }

# --- the runtimes: found and prepared once, in the temp folders ----------------------------

$goExe = Find-Exe 'go.exe'
$pythonExe = Find-Exe 'python.exe'
$nodeExe = Find-Exe 'node.exe'
$npmCmd = Find-Exe 'npm.cmd'
$npxCmd = Find-Exe 'npx.cmd'
$javaExe = Find-Exe 'java.exe'
$mvnCmd = Find-Exe 'mvn.cmd'
$dotnetExe = Find-Exe 'dotnet.exe'
$goDir = Join-Path $PSScriptRoot 'go'
$venv = Join-Path $Root 'venv'
$venvPy = Join-Path $venv 'Scripts\python.exe'
$nodeWork = Join-Path $work 'node'
$devcontainerVersion = '0.90.0'

$script:pyPrepared = $null
function Initialize-Python {
    if ($null -ne $script:pyPrepared) { return $script:pyPrepared }
    $req = Join-Path $PSScriptRoot 'python\requirements.txt'
    if (-not (Test-Path $venvPy)) {
        $r = Invoke-Tool $pythonExe @('-m', 'venv', $venv) $Root (ToolVars 'default') 300 'python-venv'
        if ($r.Code -ne 0) { $script:pyPrepared = "venv: $(Get-Reason $r)"; return $script:pyPrepared }
    }
    $r = Invoke-Tool $venvPy @('-m', 'pip', 'install', '--quiet', '-r', $req) $Root (ToolVars 'default') 600 'python-pip'
    $script:pyPrepared = if ($r.Code -eq 0) { '' } else { "pip install: $(Get-Reason $r)" }
    $script:pyPrepared
}

function PyVersion([string]$dist) {
    $r = Invoke-Tool $venvPy @('-c', "import importlib.metadata as m; print(m.version('$dist'))") $Root (ToolVars 'default') 60 ''
    $r.Out.Trim()
}

$script:nodePrepared = $null
function Initialize-Node {
    if ($null -ne $script:nodePrepared) { return $script:nodePrepared }
    New-Item -ItemType Directory -Force $nodeWork | Out-Null
    Copy-Item (Join-Path $PSScriptRoot 'node\package.json'), (Join-Path $PSScriptRoot 'node\tc_sample.mjs') $nodeWork -Force
    $r = Invoke-Tool $npmCmd @('install', '--no-package-lock') $nodeWork (ToolVars 'default') 600 'node-npm'
    $script:nodePrepared = if ($r.Code -eq 0) { '' } else { "npm install: $(Get-Reason $r)" }
    $script:nodePrepared
}

function GoModVersion([string]$module) {
    $r = Invoke-Tool $goExe @('list', '-m', '-f', '{{.Version}}', $module) $goDir (ToolVars 'default') 120 ''
    $r.Out.Trim()
}

# id -> how to run the sample: Name, Skip (why it cannot run here), Prepare, Version, Run.
$samples = [ordered]@{
    'go-sdk'    = @{
        Name = 'Docker SDK for Go (moby client)'
        Skip = { if (-not $goExe) { 'go is not on PATH' } }
        Prepare = { '' }
        Version = { "github.com/moby/moby/client $(GoModVersion 'github.com/moby/moby/client')" }
        Run = { param($v, $sec) Invoke-Tool $goExe @('test', '-count=1', '-v', '-timeout', "${sec}s", '-run', '^TestDockerSDK$', '.') $goDir $v $sec 'go-sdk' }
        TC = $false
    }
    'go-tc'     = @{
        Name = 'Testcontainers for Go'
        Skip = { if (-not $goExe) { 'go is not on PATH' } }
        Prepare = { '' }
        Version = { "testcontainers-go $(GoModVersion 'github.com/testcontainers/testcontainers-go')" }
        Run = { param($v, $sec) Invoke-Tool $goExe @('test', '-count=1', '-v', '-timeout', "${sec}s", '-run', '^TestTestcontainers$', '.') $goDir $v $sec 'go-tc' }
        TC = $true
    }
    'py-sdk'    = @{
        Name = 'Docker SDK for Python'
        Skip = { if (-not $pythonExe) { 'python is not on PATH' } }
        Prepare = { Initialize-Python }
        Version = { "docker $(PyVersion 'docker')" }
        Run = { param($v, $sec) Invoke-Tool $venvPy @((Join-Path $PSScriptRoot 'python\sdk_sample.py')) $Root $v $sec 'py-sdk' }
        TC = $false
    }
    'py-tc'     = @{
        Name = 'Testcontainers for Python'
        Skip = { if (-not $pythonExe) { 'python is not on PATH' } }
        Prepare = { Initialize-Python }
        Version = { "testcontainers $(PyVersion 'testcontainers')" }
        Run = { param($v, $sec) Invoke-Tool $venvPy @((Join-Path $PSScriptRoot 'python\tc_sample.py')) $Root $v $sec 'py-tc' }
        TC = $true
    }
    'node-tc'   = @{
        Name = 'Testcontainers for Node'
        Skip = { if (-not $nodeExe -or -not $npmCmd) { 'node or npm is not on PATH' } }
        Prepare = { Initialize-Node }
        Version = { 'testcontainers ' + (Get-Content -Raw (Join-Path $nodeWork 'node_modules\testcontainers\package.json') | ConvertFrom-Json).version }
        Run = { param($v, $sec) $v.DEBUG = 'testcontainers*'; Invoke-Tool $nodeExe @('tc_sample.mjs') $nodeWork $v $sec 'node-tc' }
        AltHost = $true
        TC = $true
    }
    'java-tc'   = @{
        Name = 'Testcontainers for Java'
        Skip = { if (-not $javaExe) { 'no java on PATH' } elseif (-not $mvnCmd) { 'no mvn on PATH (the sample is a Maven project)' } }
        Prepare = { '' }
        Version = { 'testcontainers ' + ([xml](Get-Content -Raw (Join-Path $PSScriptRoot 'java\pom.xml'))).project.dependencies.dependency[0].version }
        Run = { param($v, $sec) Invoke-Tool $mvnCmd @('-B', '-q', "-Dmaven.repo.local=$cache\m2", "-DbuildDirectory=$work\java", '-f', (Join-Path $PSScriptRoot 'java\pom.xml'), 'test') $Root $v $sec 'java-tc' }
        TC = $true
    }
    'dotnet-tc' = @{
        Name = 'Testcontainers for .NET'
        Skip = {
            if (-not $dotnetExe) { 'no dotnet on PATH' }
            else {
                $l = Invoke-Tool $dotnetExe @('--list-sdks') $Root @{} 60 ''
                if (-not $l.Out.Trim()) { 'dotnet has runtimes but no SDK (dotnet --list-sdks is empty), so it cannot build the sample' }
            }
        }
        Prepare = { '' }
        Version = { 'Testcontainers ' + ([xml](Get-Content -Raw (Join-Path $PSScriptRoot 'dotnet\compat.csproj'))).Project.ItemGroup.PackageReference.Version }
        Run = {
            param($v, $sec)
            $d = Join-Path $work 'dotnet'
            New-Item -ItemType Directory -Force $d | Out-Null
            Copy-Item (Join-Path $PSScriptRoot 'dotnet\*') $d -Force
            Invoke-Tool $dotnetExe @('run', '--project', "$d\compat.csproj") $d $v $sec 'dotnet-tc'
        }
        TC = $true
    }
}

# --- Testcontainers and Ryuk -------------------------------------------------------------

# Whether a Ryuk container started since $since, by the engine's own events.
function Test-RyukStarted([long]$since) {
    $until = [DateTimeOffset]::UtcNow.ToUnixTimeSeconds() + 1
    $r = D @('events', '--since', "$since", '--until', "$until", '--filter', 'type=container', '--filter', 'event=start', '--format', '{{.Actor.Attributes.image}}')
    $r.Out -match '(?i)ryuk'
}

# After a sample that left a container behind: Ryuk should remove everything (itself too)
# once the process is gone. Returns the seconds it took, or -1.
function Wait-Reaped([int]$limitSec) {
    $t0 = Get-Date
    while (((Get-Date) - $t0).TotalSeconds -lt $limitSec) {
        $r = D @('ps', '-aq')
        if ($r.Code -eq 0 -and -not $r.Out.Trim()) { return [int]((Get-Date) - $t0).TotalSeconds }
        Start-Sleep -Seconds 2
    }
    -1
}

# One run of a Testcontainers sample, leaving a container behind for Ryuk unless Ryuk is
# off. Returns $null when it passed, or why not; $script:ryukNote says what Ryuk did.
function Test-TcAttempt($s, [hashtable]$extra, [bool]$ryukOn) {
    $v = ToolVars 'docker-host'
    foreach ($k in $extra.Keys) { $v[$k] = $extra[$k] }
    if ($ryukOn) { $v.COMPAT_LEAK = '1' }
    $since = [DateTimeOffset]::UtcNow.ToUnixTimeSeconds() - 5
    $r = & $s.Run $v 420
    $script:tcSeconds = $r.Seconds
    if ($r.Code -ne 0) { Clear-Containers; return (Get-Reason $r) }
    if (-not $ryukOn) { $script:ryukNote = 'Ryuk off'; Clear-Containers; return $null }
    $ryuk = Test-RyukStarted $since
    $took = Wait-Reaped 120
    Clear-Containers
    if (-not $ryuk) { return 'the sample passed, but no Ryuk container started' }
    if ($took -lt 0) { return 'the sample passed and Ryuk ran, but the container left behind was still there after 120 s' }
    $script:ryukNote = "Ryuk ran and removed the container left behind in $took s"
    $null
}

function Invoke-Sample([string]$id) {
    $s = $samples[$id]
    $why = & $s.Skip
    if ($why) { Add-Result $id $s.Name 'skipped' '' 'DOCKER_HOST' $why 0; return }
    $bad = & $s.Prepare
    if ($bad) { Add-Result $id $s.Name 'fail' '' 'DOCKER_HOST' "prepare: $bad" 0; return }
    $version = & $s.Version
    if (-not $s.TC) {
        $r = & $s.Run (ToolVars 'docker-host') 420
        if ($r.Code -eq 0) { Add-Result $id $s.Name 'pass' $version 'DOCKER_HOST' '' $r.Seconds }
        else { Add-Result $id $s.Name 'fail' $version 'DOCKER_HOST' (Get-Reason $r) $r.Seconds }
        Clear-Containers
        return
    }
    # From no extra setting to giving up on Ryuk; the first that passes is what it needs.
    $attempts = New-Object System.Collections.ArrayList
    [void]$attempts.Add(@{ Label = 'DOCKER_HOST'; Vars = @{}; Ryuk = $true })
    if ($s.ContainsKey('AltHost')) {
        [void]$attempts.Add(@{ Label = "DOCKER_HOST=$altHost"; Vars = @{ DOCKER_HOST = $altHost }; Ryuk = $true })
    }
    [void]$attempts.Add(@{ Label = 'DOCKER_HOST, TESTCONTAINERS_DOCKER_SOCKET_OVERRIDE=/var/run/docker.sock'; Vars = @{ TESTCONTAINERS_DOCKER_SOCKET_OVERRIDE = '/var/run/docker.sock' }; Ryuk = $true })
    [void]$attempts.Add(@{ Label = 'DOCKER_HOST, TESTCONTAINERS_RYUK_DISABLED=true'; Vars = @{ TESTCONTAINERS_RYUK_DISABLED = 'true' }; Ryuk = $false })
    $failures = @()
    foreach ($a in $attempts) {
        $bad = Test-TcAttempt $s $a.Vars $a.Ryuk
        if (-not $bad) {
            $note = $script:ryukNote
            if ($failures.Count -gt 0) { $note += "`n" + ($failures -join "`n") }
            Add-Result $id $s.Name 'pass' $version $a.Label $note $script:tcSeconds
            return
        }
        $failures += "with $($a.Label):`n$bad"
    }
    Add-Result $id $s.Name 'fail' $version 'DOCKER_HOST' ($failures -join "`n") $script:tcSeconds
}

# --- Dev Containers ----------------------------------------------------------------------

function Invoke-DevcontainerCli([hashtable]$v, [string[]]$argv, [int]$sec, [string]$logName) {
    Invoke-Tool $npxCmd (@('--yes', "--package=@devcontainers/cli@$devcontainerVersion", 'devcontainer') + $argv) $Root $v $sec $logName
}

# `devcontainer up` of a sample from a new temp folder (a Windows bind mount), then checks
# that the file postCreateCommand wrote reached Windows, and a file from Windows reached
# the container. Returns $null on success, or why not.
function Test-Devcontainer([string]$sample, [hashtable]$v) {
    $ws = Join-Path $work "devcontainer-$sample"
    if (Test-Path $ws) { Remove-Item -Recurse -Force $ws }
    New-Item -ItemType Directory $ws | Out-Null
    Copy-Item -Recurse (Join-Path $PSScriptRoot "devcontainer\$sample\.devcontainer") $ws
    $token = [guid]::NewGuid().ToString('N')
    [IO.File]::WriteAllText((Join-Path $ws 'from-host.txt'), $token)
    $common = @('--workspace-folder', $ws, '--docker-path', $Docker)
    $up = Invoke-DevcontainerCli $v (@('up') + $common + @('--remove-existing-container')) 900 "devcontainer-$sample-up"
    $script:dcSeconds = $up.Seconds
    $json = @($up.Out -split "`r?`n" | Where-Object { $_ -match '^\{.*"outcome"' } | Select-Object -Last 1)
    if ($up.Code -ne 0 -or $json.Count -eq 0) { return "devcontainer up:`n$(Get-Reason $up)" }
    $res = $json[0] | ConvertFrom-Json
    try {
        if ($res.outcome -ne 'success') { return "devcontainer up: outcome $($res.outcome): $($res.message)" }
        $post = Join-Path $ws 'post-create.txt'
        if (-not (Test-Path $post)) { return 'postCreateCommand wrote post-create.txt in the bind mount, but it is not on the Windows side' }
        $ex = Invoke-DevcontainerCli $v (@('exec') + $common + @('cat', 'from-host.txt')) 120 "devcontainer-$sample-exec"
        if ($ex.Code -ne 0 -or $ex.Out -notmatch $token) { return "devcontainer exec:`n$(Get-Reason $ex)" }
        return $null
    } finally {
        [void](D @('rm', '-f', '-v', $res.containerId))
    }
}

function Invoke-DevcontainerRow([string]$sample, [string]$name) {
    $id = "devcontainer-$sample"
    if (-not $npxCmd) { Add-Result $id $name 'skipped' '' 'DOCKER_HOST' 'node or npx is not on PATH' 0; return }
    $script:dcSeconds = 0
    $bad = Test-Devcontainer $sample (ToolVars 'docker-host')
    $version = "@devcontainers/cli $devcontainerVersion, docker CLI $($script:dockerCliVersion)"
    if ($bad) { Add-Result $id $name 'fail' $version 'DOCKER_HOST, --docker-path' $bad $script:dcSeconds }
    else { Add-Result $id $name 'pass' $version 'DOCKER_HOST, --docker-path' '' $script:dcSeconds }
    Clear-Containers
}

# --- which setting reaches the engine (REQ-API-003) ----------------------------------------

function Test-PipeFree([string]$name) {
    -not ([IO.Directory]::GetFiles('\\.\pipe\') | Where-Object { (Split-Path -Leaf $_) -ieq $name })
}

function Invoke-SettingRows {
    # A docker context in a docker config of its own, selected there.
    [void](Invoke-Tool $Docker @('context', 'create', 'hbcompat', '--docker', "host=$dockerHost") $Root @{ DOCKER_CONFIG = $contextConfig; DOCKER_HOST = $null; DOCKER_CONTEXT = $null } 60 'settings-context-create')
    [void](Invoke-Tool $Docker @('context', 'use', 'hbcompat') $Root @{ DOCKER_CONFIG = $contextConfig; DOCKER_HOST = $null; DOCKER_CONTEXT = $null } 60 'settings-context-use')

    foreach ($mode in @('context', 'properties')) {
        if ($mode -eq 'properties') { [IO.File]::WriteAllText($tcProps, "docker.host=$dockerHost`n") }
        foreach ($id in @($samples.Keys)) {
            if (-not (Want $id)) { continue }
            $s = $samples[$id]
            if ($mode -eq 'properties' -and -not $s.TC) { continue }
            if (& $s.Skip) { continue }
            if (& $s.Prepare) { continue }
            # Only whether the setting finds the engine; Ryuk has its own row above.
            $v = ToolVars $mode
            if ($s.TC) { $v.TESTCONTAINERS_RYUK_DISABLED = 'true' }
            $r = & $s.Run $v 240
            $reached = $r.Code -eq 0
            [void]$script:settingRows.Add([pscustomobject]@{ id = $id; tool = $s.Name; setting = $mode; reached = $reached; note = $(if ($reached) { '' } else { Get-Reason $r }) })
            Write-Host ('{0,-8} {1} through {2}' -f $(if ($reached) { 'yes' } else { 'no' }), $s.Name, $mode)
            Clear-Containers
            if ($mode -eq 'properties' -and $s.ContainsKey('AltHost')) {
                # A backslash is an escape in a properties file, so it is doubled there.
                [IO.File]::WriteAllText($tcProps, "docker.host=$($altHost -replace '\\', '\\')`n")
                $v = ToolVars $mode
                $v.TESTCONTAINERS_RYUK_DISABLED = 'true'
                $r = & $s.Run $v 240
                $reached = $r.Code -eq 0
                [void]$script:settingRows.Add([pscustomobject]@{ id = $id; tool = $s.Name; setting = "properties, docker.host=$altHost"; reached = $reached; note = $(if ($reached) { '' } else { Get-Reason $r }) })
                Write-Host ('{0,-8} {1} through properties with the backslash spelling' -f $(if ($reached) { 'yes' } else { 'no' }), $s.Name)
                Clear-Containers
                [IO.File]::WriteAllText($tcProps, "docker.host=$dockerHost`n")
            }
        }
        if ($mode -eq 'context' -and $npxCmd -and (Want 'devcontainer-image')) {
            $bad = Test-Devcontainer 'image' (ToolVars 'context')
            [void]$script:settingRows.Add([pscustomobject]@{ id = 'devcontainer-image'; tool = 'Dev Containers CLI'; setting = $mode; reached = (-not $bad); note = [string]$bad })
            Write-Host ('{0,-8} Dev Containers CLI through {1}' -f $(if (-not $bad) { 'yes' } else { 'no' }), $mode)
            Clear-Containers
        }
        if ($mode -eq 'properties') { Remove-Item -Force $tcProps }
    }
}

# --- the opt-in: serving \\.\pipe\docker_engine from the isolated engine --------------------

function Invoke-OptIn {
    $o = [ordered]@{ ran = $false; why = ''; configOutput = ''; rows = @() }
    if (-not (Test-PipeFree 'docker_engine')) { $o.why = '\\.\pipe\docker_engine is served by another engine on this machine'; return $o }
    if (Test-Path (Join-Path $env:ProgramFiles 'Docker\Docker\Docker Desktop.exe')) { $o.why = 'Docker Desktop is installed'; return $o }
    [void](Hb @('stop'))
    $c = Hb @('config', 'set', 'pipe', '\\.\pipe\docker_engine') 60 'optin-config-set'
    $o.configOutput = ($c.Out + $c.Err).Trim()
    $st = Hb @('start', '--image-dir', $ImageDir, '--no-context', '--backend', 'hcs') 300 'optin-start'
    if ($st.Code -ne 0) { $o.why = "start on docker_engine: $(Get-Reason $st)"; return $o }
    $o.ran = $true
    $rows = New-Object System.Collections.ArrayList
    $cli = Invoke-Tool $Docker @('version', '--format', '{{.Server.Version}}') $Root (ToolVars 'default') 60 'optin-docker-cli'
    [void]$rows.Add([pscustomobject]@{ tool = 'docker CLI, no context'; reached = ($cli.Code -eq 0); note = $(if ($cli.Code -eq 0) { '' } else { Get-Reason $cli }) })
    foreach ($id in @($samples.Keys)) {
        if (-not (Want $id)) { continue }
        $s = $samples[$id]
        if ((& $s.Skip) -or (& $s.Prepare)) { continue }
        $r = & $s.Run (ToolVars 'default') 300
        $ok = $r.Code -eq 0
        $note = if ($ok) { '' } else { Get-Reason $r }
        Clear-Containers
        if (-not $ok -and $s.TC) {
            $v = ToolVars 'default'
            $v.TESTCONTAINERS_RYUK_DISABLED = 'true'
            $r2 = & $s.Run $v 300
            if ($r2.Code -eq 0) { $ok = $true; $note = "only with TESTCONTAINERS_RYUK_DISABLED=true; with Ryuk:`n$note" }
            Clear-Containers
        }
        [void]$rows.Add([pscustomobject]@{ tool = $s.Name; reached = $ok; note = $note })
        Write-Host ('{0,-8} {1} with nothing set, on docker_engine' -f $(if ($ok) { 'yes' } else { 'no' }), $s.Name)
    }
    $o.rows = @($rows)
    $o
}

# --- main --------------------------------------------------------------------------------

if (-not $Docker) {
    $bundled = Join-Path (Split-Path $ImageDir) 'clients\docker.exe'
    $Docker = if (Test-Path $bundled) { $bundled } else { Find-Exe 'docker.exe' }
    if (-not $Docker) { throw 'no docker CLI: pass -Docker' }
}
if (-not $Hyperbutler) {
    New-Item -ItemType Directory -Force "$Root\bin" | Out-Null
    $Hyperbutler = "$Root\bin\hyperbutler.exe"
    $b = Invoke-Tool $goExe @('build', '-o', $Hyperbutler, './cmd/hyperbutler') $repo @{} 600 'build-hyperbutler'
    if ($b.Code -ne 0) { throw "go build: $(Get-Reason $b)" }
}

$started = $false
try {
    $st = Hb @('start', '--image-dir', $ImageDir, '--pipe', $pipePath, '--no-context', '--backend', 'hcs', '--memory', $Memory, '--disk-size', $DiskSize) 300 'start'
    if ($st.Code -ne 0) { throw "hyperbutler start: $(Get-Reason $st)" }
    $started = $true
    Write-Host ($st.Out.Trim() -split "`r?`n")[0]
    Clear-Containers

    $winKey = Get-ItemProperty 'HKLM:\SOFTWARE\Microsoft\Windows NT\CurrentVersion'
    $server = (D @('version', '--format', '{{.Server.Version}} (API {{.Server.APIVersion}})')).Out.Trim()
    $script:dockerCliVersion = (D @('version', '--format', '{{.Client.Version}}')).Out.Trim()
    $hbVersion = ((Hb @('version') 30 '').Out.Trim() -split "`r?`n")[0]
    # The registry's ProductName still says Windows 10 on Windows 11.
    $winName = if ([int]$winKey.CurrentBuildNumber -ge 22000) { $winKey.ProductName -replace 'Windows 10', 'Windows 11' } else { $winKey.ProductName }
    function RuntimeVersion($exe, [string[]]$a) {
        if (-not $exe) { return 'not on PATH' }
        $r = Invoke-Tool $exe $a $Root @{} 60 ''
        (($r.Out + $r.Err).Trim() -split "`r?`n")[0]
    }
    $envInfo = [ordered]@{
        date = (Get-Date -Format 'yyyy-MM-dd')
        windows = "$winName $($winKey.DisplayVersion) build $($winKey.CurrentBuildNumber).$($winKey.UBR)"
        hyperbutler = $hbVersion
        dockerd = $server
        dockerCli = "$($script:dockerCliVersion) ($Docker)"
        pipe = $dockerHost
        go = RuntimeVersion $goExe @('version')
        python = RuntimeVersion $pythonExe @('--version')
        node = RuntimeVersion $nodeExe @('--version')
        java = RuntimeVersion $javaExe @('-version')
        maven = if ($mvnCmd) { 'on PATH' } else { 'not on PATH' }
        dotnet = if ($dotnetExe) { (Invoke-Tool $dotnetExe @('--list-runtimes') $Root @{} 60 '').Out.Trim() -replace "`r?`n", '; ' } else { 'not on PATH' }
    }

    # A guest image older than the engine boots and runs containers, but Windows bind
    # mounts fail. Say so at the top; the share may take some seconds after the start.
    $probe = Join-Path $work 'bind-probe'
    New-Item -ItemType Directory -Force $probe | Out-Null
    $envInfo.bindMount = 'not mounted after 40 s'
    for ($i = 0; $i -lt 20; $i++) {
        $b = D @('run', '--rm', '--mount', "type=bind,source=$probe,target=/w", 'alpine:3.20', 'true')
        if ($b.Code -eq 0) { $envInfo.bindMount = 'works'; break }
        $envInfo.bindMount = (Get-Reason $b)
        if ($b.Err -notmatch 'has not mounted') { break }
        Start-Sleep -Seconds 2
    }
    Write-Host "bind mount of a Windows folder: $($envInfo.bindMount)"

    foreach ($id in @($samples.Keys)) {
        if (Want $id) { Invoke-Sample $id }
    }
    if (Want 'devcontainer-image') { Invoke-DevcontainerRow 'image' 'Dev Containers CLI, image sample' }
    if (Want 'devcontainer-features') { Invoke-DevcontainerRow 'features' 'Dev Containers CLI, Dockerfile and feature sample' }

    if (-not $SkipSettings) { Invoke-SettingRows }
    if (-not $SkipOptIn) { $script:optIn = Invoke-OptIn }
} finally {
    if ($started) {
        $stop = Hb @('stop') 180 'stop'
        Write-Host "hyperbutler stop: exit $($stop.Code)"
    }
}

# Not drivable here; the settings are in docs/tools.md.
Add-Result 'jetbrains' 'JetBrains IDEs' 'not automated' '' 'Docker connection: TCP socket, Engine API URL npipe:////./pipe/<name>' 'a GUI setting; cannot be driven from a script' 0
Add-Result 'vscode' 'VS Code Docker / Dev Containers extensions' 'not automated' $(if (Find-Exe 'code.cmd') { 'code on PATH' } else { '' }) 'selected docker context, or docker.environment / containers.environment DOCKER_HOST in settings.json' 'a GUI extension; the CLI row above is the same container path' 0

# --- report ------------------------------------------------------------------------------

$md = New-Object System.Text.StringBuilder
function Line([string]$s = '') { [void]$md.AppendLine($s) }
function Cell([string]$s) { ($s -replace '\|', '\|') -replace "\r?\n", '<br>' }
Line "# bench/compat-tools, $($envInfo.date)"
Line
foreach ($k in $envInfo.Keys) { Line "- ${k}: $($envInfo[$k])" }
Line
Line '| Tool | Result | Version | Setting | Note | Seconds |'
Line '|---|---|---|---|---|---|'
foreach ($r in $script:results) { Line "| $(Cell $r.tool) | $($r.status) | $(Cell $r.version) | $(Cell $r.setting) | $(Cell $r.note) | $($r.seconds) |" }
if ($script:settingRows.Count -gt 0) {
    Line
    Line '## Without DOCKER_HOST'
    Line
    Line '| Tool | Setting | Reached the engine | Note |'
    Line '|---|---|---|---|'
    foreach ($r in $script:settingRows) { Line "| $(Cell $r.tool) | $($r.setting) | $(if ($r.reached) { 'yes' } else { 'no' }) | $(Cell $r.note) |" }
}
if ($script:optIn) {
    Line
    Line '## The opt-in, config set pipe \\.\pipe\docker_engine'
    Line
    if (-not $script:optIn.ran) { Line "Not run: $($script:optIn.why)" }
    else {
        Line 'Output of config set:'
        Line
        Line '```'
        Line $script:optIn.configOutput
        Line '```'
        Line
        Line '| Tool, nothing set | Reached the engine | Note |'
        Line '|---|---|---|'
        foreach ($r in $script:optIn.rows) { Line "| $(Cell $r.tool) | $(if ($r.reached) { 'yes' } else { 'no' }) | $(Cell $r.note) |" }
    }
}
$report = $md.ToString()
[IO.File]::WriteAllText((Join-Path $Out 'results.md'), $report)
$all = [ordered]@{ environment = $envInfo; tools = @($script:results); withoutDockerHost = @($script:settingRows); optIn = $script:optIn }
[IO.File]::WriteAllText((Join-Path $Out 'results.json'), ($all | ConvertTo-Json -Depth 6))
Write-Host ''
Write-Host $report
Write-Host "written to $Out"

$failed = @($script:results | Where-Object { $_.status -eq 'fail' })
if ($failed.Count -gt 0) { Write-Host "$($failed.Count) tool(s) failed"; exit 1 }
