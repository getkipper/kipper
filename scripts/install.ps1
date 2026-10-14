# Kipper CLI installer for Windows
# Usage: irm https://getkipper.com/install.ps1 | iex
#
# Downloads kip-windows-amd64.exe from GitHub Releases, checks it against the
# release's checksums, installs it as kip.exe, and adds its folder to your user
# PATH. The default installation requires no administrator rights.
#
# Set KIP_VERSION to select a release (default: latest) or KIP_INSTALL_DIR to
# select a folder (default: %LOCALAPPDATA%\Programs\kip). Set KIP_GITHUB_TOKEN
# to authenticate the latest-release lookup.

# Use return on failure to keep the caller's PowerShell session open when
# invoked through Invoke-Expression.
& {
  $ErrorActionPreference = 'Stop'
  $ProgressPreference = 'SilentlyContinue'

  function Fail([string[]]$lines) {
    Write-Host ''
    foreach ($line in $lines) { Write-Host "  $line" -ForegroundColor Red }
    Write-Host ''
  }

  # Enable TLS 1.2 for older PowerShell sessions that select protocols explicitly.
  # Preserve system defaults and restore the session setting in finally.
  $originalProtocol = [Net.ServicePointManager]::SecurityProtocol
  try {
    $tls12 = [Net.SecurityProtocolType]::Tls12
    if ($originalProtocol -ne [Net.SecurityProtocolType]::SystemDefault -and -not ($originalProtocol -band $tls12)) {
      [Net.ServicePointManager]::SecurityProtocol = $originalProtocol -bor $tls12
    }

    $repo = 'getkipper/kipper'
    $binary = 'kip-windows-amd64.exe'
    $version = if ($env:KIP_VERSION) { $env:KIP_VERSION } else { 'latest' }
    $localAppData = if ($env:LOCALAPPDATA) { $env:LOCALAPPDATA } else { [Environment]::GetFolderPath('LocalApplicationData') }
    $requestedDir = if ($env:KIP_INSTALL_DIR) { $env:KIP_INSTALL_DIR } else { Join-Path $localAppData 'Programs\kip' }

    # PATH needs an absolute filesystem path. Reject semicolons and percent
    # signs to prevent entry splitting or environment-variable expansion.
    $installDir = [IO.Path]::GetFullPath($ExecutionContext.SessionState.Path.GetUnresolvedProviderPathFromPSPath($requestedDir))
    if ($installDir.Length -gt [IO.Path]::GetPathRoot($installDir).Length) {
      $installDir = $installDir.TrimEnd('\', '/')
    }
    if ($installDir.Contains(';') -or $installDir.Contains('%')) {
      Fail @("Error: $installDir contains ';' or '%', which can change how PATH is interpreted.", 'Choose another folder with $env:KIP_INSTALL_DIR.')
      return
    }
    $target = Join-Path $installDir 'kip.exe'
    if (Test-Path -LiteralPath $target -PathType Container) {
      Fail @("Error: $target is a folder. Move it aside or choose another folder with `$env:KIP_INSTALL_DIR.")
      return
    }

    # The x64 binary also supports Windows 11 ARM64 through emulation.
    # Check the OS architecture even when PowerShell is a 32-bit process.
    if ($env:OS -eq 'Windows_NT') {
      $arch = if ($env:PROCESSOR_ARCHITEW6432) { $env:PROCESSOR_ARCHITEW6432 } else { $env:PROCESSOR_ARCHITECTURE }
      $build = [Environment]::OSVersion.Version.Build
      if (-not ($arch -eq 'AMD64' -or ($arch -eq 'ARM64' -and $build -ge 22000))) {
        Fail @("Error: kip for Windows runs on x64 Windows, and on Windows 11 on ARM. This system reports $arch, Windows build $build.")
        return
      }
    }

    Write-Host ''
    Write-Host '  Kipper CLI Installer'
    Write-Host ''
    Write-Host "  Binary:  $binary"

    if ($version -eq 'latest') {
      Write-Host '  Fetching latest version...'
      try {
        $headers = @{}
        if ($env:KIP_GITHUB_TOKEN) { $headers['Authorization'] = "Bearer $env:KIP_GITHUB_TOKEN" }
        $version = (Invoke-RestMethod -UseBasicParsing -Headers $headers "https://api.github.com/repos/$repo/releases/latest").tag_name
      } catch {
        $version = $null
      }
      if (-not $version) {
        Fail @('Error: could not determine the latest version.', 'Set $env:KIP_VERSION = "v0.29.0" to install a specific version.')
        return
      }
    }
    Write-Host "  Version: $version"
    Write-Host ''

    $releaseUrl = "https://github.com/$repo/releases/download/$version"
    $tmp = Join-Path ([IO.Path]::GetTempPath()) ("kip-install-" + [Guid]::NewGuid().ToString('N'))
    New-Item -ItemType Directory -Path $tmp | Out-Null
    try {
      Write-Host '  Downloading...'
      $exe = Join-Path $tmp 'kip.exe'
      try {
        Invoke-WebRequest -UseBasicParsing "$releaseUrl/$binary" -OutFile $exe
      } catch {
        Fail @("Error: download failed. Check that version $version exists at", "https://github.com/$repo/releases")
        return
      }
      $sums = Join-Path $tmp 'checksums.txt'
      try {
        Invoke-WebRequest -UseBasicParsing "$releaseUrl/checksums.txt" -OutFile $sums
      } catch {
        Fail @("Error: could not fetch checksums.txt for $version to verify the download.", 'Retry the installer. If the file is missing from the release, report it at', "https://github.com/$repo/issues")
        return
      }

      Write-Host '  Verifying...'
      # Require one checksum for the exact filename to avoid ambiguous matches.
      $found = @(Get-Content -LiteralPath $sums | Where-Object { $_ -match "^[0-9a-f]{64}\s+$([regex]::Escape($binary))$" })
      if ($found.Count -ne 1) {
        Fail @("Error: checksums.txt for $version has $($found.Count) entries for $binary.", 'Refusing to install an unverified binary.')
        return
      }
      $expected = ($found[0] -split '\s+')[0]
      $actual = (Get-FileHash -Algorithm SHA256 -LiteralPath $exe).Hash.ToLowerInvariant()
      if ($actual -ne $expected) {
        Fail @("Error: checksum mismatch for $binary.", "  expected: $expected", "  actual:   $actual", 'The download failed verification. Nothing was installed.')
        return
      }

      try {
        New-Item -ItemType Directory -Force -Path $installDir | Out-Null
        Copy-Item -Force -LiteralPath $exe -Destination $target
      } catch {
        Fail @("Error: could not install to $installDir.", 'If kip is running, close it and run this again, or choose another folder:', '  $env:KIP_INSTALL_DIR = "C:\Tools"; irm https://getkipper.com/install.ps1 | iex')
        return
      }
    } finally {
      Remove-Item -Recurse -Force -LiteralPath $tmp -ErrorAction SilentlyContinue
    }

    Write-Host "  kip $version installed to $target" -ForegroundColor Green
    Write-Host ''

    if ($env:OS -eq 'Windows_NT') {
      # Preserve variable references such as %JAVA_HOME%\bin in the stored PATH.
      $key = [Microsoft.Win32.Registry]::CurrentUser.CreateSubKey('Environment')
      try {
        $raw = [string]$key.GetValue('Path', '', [Microsoft.Win32.RegistryValueOptions]::DoNotExpandEnvironmentNames)
        $kind = if ($key.GetValueNames() -contains 'Path') { $key.GetValueKind('Path') } else { [Microsoft.Win32.RegistryValueKind]::ExpandString }
        $present = @($raw -split ';' | Where-Object { $_ } | ForEach-Object { [Environment]::ExpandEnvironmentVariables($_).TrimEnd('\') }) -contains $installDir.TrimEnd('\')
        if (-not $present) {
          $key.SetValue('Path', $(if ($raw) { "$($raw.TrimEnd(';'));$installDir" } else { $installDir }), $kind)
          # Use a temporary variable to broadcast the environment change after
          # writing PATH directly to the registry.
          [Environment]::SetEnvironmentVariable('KIP_INSTALLER_NOTICE', '1', 'User')
          [Environment]::SetEnvironmentVariable('KIP_INSTALLER_NOTICE', $null, 'User')
          Write-Host "  Added $installDir to your user PATH."
        }
      } finally {
        $key.Close()
      }
      if (@($env:Path -split ';' | ForEach-Object { $_.TrimEnd('\') }) -notcontains $installDir.TrimEnd('\')) {
        $env:Path = "$env:Path;$installDir"
      }
    }

    Write-Host ''
    Write-Host '  Get started:'
    Write-Host '    kip install --host <your-server-ip>'
    Write-Host ''
  } catch {
    Fail @("Error: $($_.Exception.Message)", 'Report a problem at https://github.com/getkipper/kipper/issues')
  } finally {
    [Net.ServicePointManager]::SecurityProtocol = $originalProtocol
  }
}
