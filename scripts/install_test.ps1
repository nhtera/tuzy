# Tests scripts/install.ps1 (Windows only) against a local mirror of a GoReleaser snapshot (dist/):
# good install, user PATH update, reinstall over a running tuzy.exe, tampered archive.
# Runs the installer under the same PowerShell that runs this file, so CI covers 5.1 and 7:
#   goreleaser release --snapshot --clean --skip=sign --single-target
#   pwsh -File scripts/install_test.ps1; powershell -ExecutionPolicy Bypass -File scripts/install_test.ps1
$ErrorActionPreference = 'Stop'
$root = Split-Path -Parent $PSScriptRoot
$dist = Join-Path $root 'dist'
if (-not (Test-Path (Join-Path $dist 'checksums.txt'))) { throw 'run a goreleaser snapshot first' }
$shell = (Get-Process -Id $PID).Path

$version = $null
foreach ($line in Get-Content (Join-Path $dist 'checksums.txt')) {
  if ($line -match 'tuzy_(.+)_windows_[a-z0-9]+\.zip$') { $version = $Matches[1] }
}
if (-not $version) { throw 'no windows archive in dist/checksums.txt' }
$work = Join-Path ([IO.Path]::GetTempPath()) ('tuzy-install-test-' + [Guid]::NewGuid().ToString('N'))
$rel = Join-Path $work "releases\download\v$version"
New-Item -ItemType Directory -Path $rel | Out-Null
Copy-Item (Join-Path $dist 'checksums.txt') $rel
Copy-Item (Join-Path $dist 'tuzy_*_windows_*.zip') $rel
$bin = Join-Path $work 'bin'

$port = 8792
if ($env:TUZY_TEST_PORT) { $port = [int]$env:TUZY_TEST_PORT }
$server = Start-Process python -ArgumentList '-m', 'http.server', "$port", '--bind', '127.0.0.1', '--directory', $work -PassThru -WindowStyle Hidden
$lock = $null
$regKey = [Microsoft.Win32.Registry]::CurrentUser.OpenSubKey('Environment', $true)
$savedPath = $regKey.GetValue('Path', $null, [Microsoft.Win32.RegistryValueOptions]::DoNotExpandEnvironmentNames)
$savedKind = if ($null -ne $savedPath) { $regKey.GetValueKind('Path') } else { $null }

function Fail($msg) { throw "FAIL $msg" }

# Runs the installer in a child process; returns its combined output, sets $script:code.
function Invoke-Installer([switch]$ModifyPath) {
  $env:TUZY_TEST_RELEASES_URL = "http://127.0.0.1:$port/releases"
  $env:TUZY_VERSION = "v$version"
  $env:TUZY_INSTALL_DIR = $bin
  $env:TUZY_NO_MODIFY_PATH = if ($ModifyPath) { '' } else { '1' }
  $ErrorActionPreference = 'Continue'
  $out = & $shell -NoProfile -ExecutionPolicy Bypass -File (Join-Path $root 'scripts\install.ps1') 2>&1 | Out-String
  $script:code = $LASTEXITCODE
  return $out
}

try {
  $up = $false
  for ($i = 0; $i -lt 50 -and -not $up; $i++) {
    try { Invoke-WebRequest -UseBasicParsing "http://127.0.0.1:$port/" | Out-Null; $up = $true } catch { Start-Sleep -Milliseconds 100 }
  }
  if (-not $up) { throw "mirror on port $port didn't start" }
  $exe = Join-Path $bin 'tuzy.exe'

  Write-Host '== good install'
  $out = Invoke-Installer
  if ($code -ne 0) { Fail "good install: $out" }
  if (-not ((& $exe version) -match [regex]::Escape($version))) { Fail 'installed tuzy.exe reports the wrong version' }
  Write-Host 'PASS good install'

  Write-Host '== user PATH'
  $out = Invoke-Installer -ModifyPath
  if ($code -ne 0) { Fail "PATH install: $out" }
  $userPath = [string]$regKey.GetValue('Path', '', [Microsoft.Win32.RegistryValueOptions]::DoNotExpandEnvironmentNames)
  if (-not ($userPath -split ';' -contains $bin)) { Fail "user PATH lacks $bin" }
  $null = Invoke-Installer -ModifyPath # a second run must not add it again
  $userPath = [string]$regKey.GetValue('Path', '', [Microsoft.Win32.RegistryValueOptions]::DoNotExpandEnvironmentNames)
  if (@($userPath -split ';' | Where-Object { $_ -eq $bin }).Count -ne 1) { Fail "user PATH lists $bin more than once" }
  Write-Host 'PASS user PATH'

  Write-Host '== reinstall over a running tuzy.exe'
  # `config add-token -` blocks reading stdin until EOF, keeping the image locked like a service would.
  $psi = New-Object Diagnostics.ProcessStartInfo $exe, 'config add-token -'
  $psi.UseShellExecute = $false
  $psi.RedirectStandardInput = $true
  $lock = [Diagnostics.Process]::Start($psi)
  Start-Sleep -Milliseconds 500
  if ($lock.HasExited) {
    Write-Host 'SKIP reinstall over a running tuzy.exe (it exited early)'
  } else {
    $out = Invoke-Installer
    if ($code -ne 0) { Fail "reinstall while running: $out" }
    if (-not (Get-ChildItem -Path $bin -Filter 'tuzy.exe.old-*')) { Fail 'the running tuzy.exe was not moved aside' }
    Write-Host 'PASS reinstall over a running tuzy.exe'
  }

  Write-Host '== tampered archive'
  # Tamper with every archive: the installer picks by native architecture.
  Get-ChildItem -Path $rel -Filter '*.zip' | ForEach-Object { Add-Content -LiteralPath $_.FullName -Value 'x' -NoNewline }
  $out = Invoke-Installer
  if ($code -eq 0) { Fail 'tampered archive installed' }
  if ($out -notmatch 'sha256 mismatch') { Fail "tampered archive: unexpected error: $out" }
  Write-Host 'PASS tampered archive refused'
} finally {
  if ($lock -and -not $lock.HasExited) { $lock.Kill() }
  if ($null -ne $savedPath) { $regKey.SetValue('Path', $savedPath, $savedKind) } else { $regKey.DeleteValue('Path', $false) }
  $regKey.Close()
  Stop-Process -Id $server.Id -Force -ErrorAction SilentlyContinue
  Start-Sleep -Milliseconds 300
  Remove-Item -LiteralPath $work -Recurse -Force -ErrorAction SilentlyContinue
}
