# Oopsy installer for Windows:
#   irm https://sohan-bhat.github.io/oopsy/install.ps1 | iex
# Installs oops.exe and oops-db.exe into %LOCALAPPDATA%\Programs\oopsy and
# adds that folder to your user PATH. Works in Windows PowerShell 5.1 and
# PowerShell 7. Pin a version first with $env:OOPS_VERSION = '0.2.0', and
# pick a folder with $env:OOPSY_INSTALL_DIR.

function Install-Oopsy {
  $ErrorActionPreference = 'Stop'
  $ProgressPreference = 'SilentlyContinue' # Invoke-WebRequest is far faster without the progress bar
  $repo = 'sohan-bhat/oopsy'
  if ($PSVersionTable.PSVersion.Major -lt 6) {
    [Net.ServicePointManager]::SecurityProtocol = [Net.ServicePointManager]::SecurityProtocol -bor [Net.SecurityProtocolType]::Tls12
  }

  $arch = $env:PROCESSOR_ARCHITEW6432
  if (-not $arch) { $arch = $env:PROCESSOR_ARCHITECTURE }
  try { $arch = [System.Runtime.InteropServices.RuntimeInformation]::OSArchitecture.ToString() } catch { }
  switch -regex ($arch) {
    '^(AMD64|X64)$' { $arch = 'amd64' }
    '^ARM64$' { $arch = 'arm64' }
    default { throw "oopsy install: unsupported CPU: $arch" }
  }

  $version = $env:OOPS_VERSION
  if (-not $version) { $version = $env:OOPSY_VERSION }
  if ($version) {
    $version = 'v' + $version.TrimStart('v')
  } else {
    try {
      $version = (Invoke-RestMethod "https://api.github.com/repos/$repo/releases/latest" -Headers @{ 'User-Agent' = 'oopsy-install' }).tag_name
    } catch { $version = $null }
    if (-not $version) {
      # The API limits anonymous calls; the release page redirect has the tag too.
      $r = Invoke-WebRequest "https://github.com/$repo/releases/latest" -UseBasicParsing
      $final = $null
      try { $final = $r.BaseResponse.ResponseUri.AbsoluteUri } catch { }
      if (-not $final) { try { $final = $r.BaseResponse.RequestMessage.RequestUri.AbsoluteUri } catch { } }
      if ($final -match '/tag/([^/]+)$') { $version = $Matches[1] }
    }
    if (-not $version) { throw 'oopsy install: could not find the latest release; pin one with $env:OOPS_VERSION' }
  }
  $plain = $version.TrimStart('v')
  $archive = "oopsy_${plain}_windows_${arch}.zip"
  $base = "https://github.com/$repo/releases/download/$version"

  $tmp = Join-Path ([IO.Path]::GetTempPath()) ("oopsy-" + [guid]::NewGuid())
  New-Item -ItemType Directory $tmp | Out-Null
  try {
    Write-Host "Downloading Oopsy $version for windows/$arch"
    Invoke-WebRequest "$base/$archive" -OutFile (Join-Path $tmp $archive) -UseBasicParsing
    Invoke-WebRequest "$base/checksums.txt" -OutFile (Join-Path $tmp 'checksums.txt') -UseBasicParsing

    $line = Get-Content (Join-Path $tmp 'checksums.txt') | Where-Object { $_ -match "\s$([regex]::Escape($archive))$" } | Select-Object -First 1
    if (-not $line) { throw "oopsy install: no checksum listed for $archive" }
    $want = ($line -split '\s+')[0].ToLower()
    $got = (Get-FileHash (Join-Path $tmp $archive) -Algorithm SHA256).Hash.ToLower()
    if ($want -ne $got) { throw "oopsy install: checksum mismatch for $archive" }

    $dir = $env:OOPSY_INSTALL_DIR
    if (-not $dir) { $dir = Join-Path $env:LOCALAPPDATA 'Programs\oopsy' }
    New-Item -ItemType Directory -Force $dir | Out-Null
    Expand-Archive (Join-Path $tmp $archive) -DestinationPath (Join-Path $tmp 'x') -Force
    foreach ($exe in 'oops.exe', 'oops-db.exe') {
      Copy-Item (Join-Path $tmp "x\$exe") (Join-Path $dir $exe) -Force
    }
    Write-Host "Installed oops and oops-db to $dir"

    $userPath = [Environment]::GetEnvironmentVariable('Path', 'User')
    $parts = @($userPath -split ';' | Where-Object { $_ })
    if ($parts -notcontains $dir) {
      [Environment]::SetEnvironmentVariable('Path', (($parts + $dir) -join ';'), 'User')
      Write-Host "Added $dir to your user PATH"
    }
    if (($env:Path -split ';') -notcontains $dir) { $env:Path = "$env:Path;$dir" }
    Write-Host ''
    Write-Host 'Next: run  oops init  to turn on the PowerShell, Git Bash and AI agent hooks.'
  }
  finally {
    Remove-Item -Recurse -Force $tmp -ErrorAction SilentlyContinue
  }
}

Install-Oopsy
