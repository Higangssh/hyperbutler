# Fuzzes every Fuzz target in every package for $env:FUZZTIME each. The fuzz smoke job in
# ci.yml runs it short on every push and PR; fuzz-weekly.yml runs it long once a week.
#
#   $env:FUZZTIME = '10s'; $env:FUZZ_MIN_TARGETS = '78'; ./scripts/ci/fuzz-all.ps1
#
# FUZZ_MIN_TARGETS fails the run if fewer targets are found, so a renamed or mis-tagged
# target can't make it pass silently. Raise it whenever a Fuzz target is added.
# `go test -fuzz` accepts one target in one package per run, so discover them first.
# Seed corpora already run as plain tests in the test job.
$ErrorActionPreference = 'Stop'

if (-not $env:FUZZTIME) { throw 'FUZZTIME is not set' }
if (-not $env:FUZZ_MIN_TARGETS) { throw 'FUZZ_MIN_TARGETS is not set' }

$pkgs = @(go list ./...)
if ($LASTEXITCODE -ne 0) { throw "go list ./... failed (exit $LASTEXITCODE)" }
if ($pkgs.Count -eq 0) { throw 'go list ./... matched no packages' }

$ran = 0
$failed = @()
foreach ($pkg in $pkgs) {
  # -list prints matching test names one per line, then an "ok" summary line.
  # A build failure stops the run instead of looking like "no targets".
  $out = @(go test -list '^Fuzz' $pkg)
  if ($LASTEXITCODE -ne 0) { $out | Write-Host; throw "go test -list failed for $pkg" }
  $targets = @($out | Where-Object { $_ -match '^Fuzz\w*$' })
  foreach ($target in $targets) {
    Write-Host "::group::$pkg $target"
    go test -run '^$' -fuzz "^$target`$" -fuzztime $env:FUZZTIME $pkg
    $rc = $LASTEXITCODE
    Write-Host '::endgroup::'
    $ran++
    if ($rc -ne 0) {
      Write-Host "::error title=fuzz::$target in $pkg failed (exit $rc)"
      $failed += "$pkg $target"
    }
  }
}

if ($ran -lt [int]$env:FUZZ_MIN_TARGETS) {
  Write-Host "::error title=fuzz::Found $ran Fuzz target(s), expected at least $env:FUZZ_MIN_TARGETS."
  exit 1
}
Write-Host "Fuzzed $ran target(s) across $($pkgs.Count) package(s) for $env:FUZZTIME each; $($failed.Count) failed."
if ($failed.Count -gt 0) { exit 1 }
