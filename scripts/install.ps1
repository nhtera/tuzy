# tuzy installer for Windows: irm https://tuzy.dev/install.ps1 | iex
#
# Downloads the tuzy release for this architecture from GitHub, checks the archive's sha256 against
# checksums.txt and installs tuzy.exe to $env:TUZY_INSTALL_DIR (default %LOCALAPPDATA%\Programs\tuzy),
# adding that directory to the user PATH (skip with $env:TUZY_NO_MODIFY_PATH = 1). Pin a version
# with $env:TUZY_VERSION = 'v1.2.3'. No admin rights needed.
#
# Windows has no built-in ed25519, so this relies on HTTPS + sha256 (like install.sh with an openssl
# that can't verify ed25519); every `tuzy update` verifies the release signature.
# Runs on Windows PowerShell 5.1 and PowerShell 7+. Keep this file ASCII: 5.1 reads BOM-less
# scripts as ANSI.

# Everything runs inside one script block: `iex` evaluates in the caller's session, so this keeps
# variables and preferences out of it, and a truncated download fails to parse and runs nothing.
& {
  $ErrorActionPreference = 'Stop'
  $ProgressPreference = 'SilentlyContinue' # Windows PowerShell's progress bar slows downloads badly

  $repo = 'https://github.com/nhtera/tuzy/releases'
  # Test hook (scripts/install_test.ps1): a local release mirror.
  if ($env:TUZY_TEST_RELEASES_URL) { $repo = $env:TUZY_TEST_RELEASES_URL }

  # Windows PowerShell 5.1 can default to TLS 1.0/1.1, which GitHub refuses.
  [Net.ServicePointManager]::SecurityProtocol = [Net.ServicePointManager]::SecurityProtocol -bor [Net.SecurityProtocolType]::Tls12

  # The machine's native architecture: the registry value stays right when this PowerShell itself
  # runs emulated (x64 on ARM64).
  $arch = $null
  try { $arch = (Get-ItemProperty 'HKLM:\SYSTEM\CurrentControlSet\Control\Session Manager\Environment').PROCESSOR_ARCHITECTURE } catch { }
  if (-not $arch) { $arch = $env:PROCESSOR_ARCHITECTURE }
  switch ($arch) {
    'AMD64' { $arch = 'amd64' }
    'ARM64' { $arch = 'arm64' }
    default { throw "tuzy install: unsupported architecture $arch (tuzy ships amd64 and arm64)" }
  }

  $version = $env:TUZY_VERSION
  if (-not $version) {
    # The first redirect of releases/latest names the newest stable tag (no API rate limit).
    $req = [Net.WebRequest]::Create("$repo/latest/download/checksums.txt")
    $req.AllowAutoRedirect = $false
    try { $res = $req.GetResponse() } catch { throw "tuzy install: cannot reach GitHub ($($_.Exception.Message))" }
    $loc = $res.Headers['Location']
    $res.Close()
    if ($loc -notmatch '/releases/download/(v[^/]+)/') { throw 'tuzy install: no published release found' }
    $version = $Matches[1]
  }
  if (-not $version.StartsWith('v')) { $version = "v$version" }

  $asset = "tuzy_$($version.Substring(1))_windows_$arch.zip"
  $tmp = Join-Path ([IO.Path]::GetTempPath()) ('tuzy-install-' + [Guid]::NewGuid().ToString('N'))
  New-Item -ItemType Directory -Path $tmp | Out-Null
  try {
    Write-Host "Downloading tuzy $version for windows/$arch..."
    foreach ($f in @('checksums.txt', $asset)) {
      try { Invoke-WebRequest -UseBasicParsing -Uri "$repo/download/$version/$f" -OutFile (Join-Path $tmp $f) }
      catch { throw "tuzy install: download failed: $f ($($_.Exception.Message))" }
    }

    # sha256 of the archive, against its checksums.txt line ("<hash>  <name>" or "<hash> *<name>").
    $want = $null
    foreach ($line in Get-Content -LiteralPath (Join-Path $tmp 'checksums.txt')) {
      $parts = $line.Trim() -split '\s+', 2
      if ($parts.Count -eq 2 -and $parts[1].TrimStart('*') -eq $asset) { $want = $parts[0] }
    }
    if (-not $want) { throw "tuzy install: $asset is not listed in checksums.txt" }
    $got = (Get-FileHash -Algorithm SHA256 -LiteralPath (Join-Path $tmp $asset)).Hash
    if ($got -ne $want) { throw "tuzy install: sha256 mismatch for $asset" }
    Write-Host 'checksum verified'

    $unpacked = Join-Path $tmp 'unpacked'
    Expand-Archive -LiteralPath (Join-Path $tmp $asset) -DestinationPath $unpacked
    $bin = Join-Path $unpacked 'tuzy.exe'
    if (-not (Test-Path -LiteralPath $bin)) { throw 'tuzy install: archive has no tuzy.exe' }
    # Run it before touching an existing install. 'Continue' here: Windows PowerShell turns any
    # native stderr line into a terminating error under 'Stop'.
    $out = $null
    try { $out = & { $ErrorActionPreference = 'Continue'; & $bin version 2>$null } } catch { }
    if ($LASTEXITCODE -ne 0 -or -not $out) { throw "tuzy install: tuzy.exe doesn't run on this system" }

    $dir = $env:TUZY_INSTALL_DIR
    if (-not $dir) { $dir = Join-Path $env:LOCALAPPDATA 'Programs\tuzy' }
    $dir = $ExecutionContext.SessionState.Path.GetUnresolvedProviderPathFromPSPath($dir).TrimEnd('\')
    try { New-Item -ItemType Directory -Force -Path $dir | Out-Null } catch { throw "tuzy install: cannot create $dir (set TUZY_INSTALL_DIR)" }
    $exe = Join-Path $dir 'tuzy.exe'

    # Copy next to the target, then swap. A running tuzy.exe (e.g. the service) can be renamed but
    # not overwritten: it moves aside to tuzy.exe.old-<hex>, which `tuzy update` cleans up later.
    $new = Join-Path $dir ".tuzy-install-$PID.exe"
    $old = $null
    try {
      Copy-Item -LiteralPath $bin -Destination $new -Force
      if (Test-Path -LiteralPath $exe) {
        try { Remove-Item -LiteralPath $exe -Force }
        catch {
          $old = "$exe.old-" + [Guid]::NewGuid().ToString('N').Substring(0, 8)
          Move-Item -LiteralPath $exe -Destination $old
        }
      }
      Move-Item -LiteralPath $new -Destination $exe
    } catch {
      if ($old -and -not (Test-Path -LiteralPath $exe)) { Move-Item -LiteralPath $old -Destination $exe -ErrorAction SilentlyContinue }
      Remove-Item -LiteralPath $new -Force -ErrorAction SilentlyContinue
      throw "tuzy install: cannot write to $dir ($($_.Exception.Message)); set TUZY_INSTALL_DIR"
    }

    Write-Host "installed $out to $exe"

    $onPath = @($env:Path -split ';' | Where-Object { $_.TrimEnd('\') -eq $dir })
    if ($env:TUZY_NO_MODIFY_PATH) {
      if (-not $onPath) { Write-Host "Add it to your PATH: $dir" }
    } else {
      # Edit the raw registry value: [Environment]::SetEnvironmentVariable would expand entries
      # like %USERPROFILE%\bin and store the whole user PATH as a plain string.
      $key = [Microsoft.Win32.Registry]::CurrentUser.OpenSubKey('Environment', $true)
      try {
        $userPath = [string]$key.GetValue('Path', '', [Microsoft.Win32.RegistryValueOptions]::DoNotExpandEnvironmentNames)
        $entries = @($userPath -split ';' | Where-Object { $_ })
        $listed = $entries | Where-Object { [Environment]::ExpandEnvironmentVariables($_).TrimEnd('\') -eq $dir }
        if (-not $listed) {
          $key.SetValue('Path', (($entries + $dir) -join ';'), [Microsoft.Win32.RegistryValueKind]::ExpandString)
          # Setting a user variable broadcasts WM_SETTINGCHANGE, so new terminals pick up the PATH.
          [Environment]::SetEnvironmentVariable('TUZY_INSTALL_REFRESH', '1', 'User')
          [Environment]::SetEnvironmentVariable('TUZY_INSTALL_REFRESH', $null, 'User')
          Write-Host "added $dir to your user PATH"
        }
      } finally { $key.Close() }
      if (-not $onPath) { $env:Path = "$dir;$env:Path" } # this session, too
    }
    Write-Host 'Next: tuzy login; tuzy http 3000'
  } finally {
    Remove-Item -LiteralPath $tmp -Recurse -Force -ErrorAction SilentlyContinue
  }
}
