param()

$ErrorActionPreference = 'Stop'
$root = (Resolve-Path (Join-Path $PSScriptRoot '..')).Path
if ([Environment]::OSVersion.Platform -ne [PlatformID]::Win32NT) { throw 'Windows required' }
$version = (Get-Content (Join-Path $root 'VERSION') -Raw).Trim()
if ($version -notmatch '^[0-9A-Za-z][0-9A-Za-z._+-]*$') { throw 'Invalid VERSION' }

$arch = switch ([System.Runtime.InteropServices.RuntimeInformation]::ProcessArchitecture.ToString()) {
  'X64' { 'amd64' }
  'Arm64' { 'arm64' }
  default { throw "Unsupported Windows architecture: $_" }
}
if (-not $env:CC -and -not (Get-Command gcc -ErrorAction SilentlyContinue)) {
  throw 'A CGO C compiler is required; install MinGW GCC or set CC before packaging.'
}
$archiveName = "temporal_cli_1.9.1_windows_${arch}.zip"
$digest = switch ($arch) {
  'amd64' { 'babb65844835045c91fb98930fe10b81de28db80c8c609180473d2c0b18c7589' }
  'arm64' { '709dd23d856020b318676064ea0141adc35eae3716360265481b13e54988e12f' }
}

$runRoot = Join-Path $root 'run'
$distRoot = Join-Path $root 'dist'
New-Item -ItemType Directory -Force -Path $runRoot, $distRoot | Out-Null
$scratch = Join-Path $runRoot ('.package-windows-' + [Guid]::NewGuid().ToString('N'))
New-Item -ItemType Directory -Path $scratch | Out-Null
try {
  $archive = if ($env:TEMPORAL_ARCHIVE) { $env:TEMPORAL_ARCHIVE } else { Join-Path $scratch $archiveName }
  if (-not (Test-Path -LiteralPath $archive -PathType Leaf)) {
    Invoke-WebRequest -Uri "https://github.com/temporalio/cli/releases/download/v1.9.1/$archiveName" -OutFile $archive
  }
  $actualDigest = (Get-FileHash -Algorithm SHA256 -LiteralPath $archive).Hash.ToLowerInvariant()
  if ($actualDigest -ne $digest) { throw "Temporal archive checksum mismatch: $actualDigest" }

  $name = "stable-$version-windows-$arch"
  $package = Join-Path $distRoot $name
  if (Test-Path -LiteralPath $package) { Remove-Item -LiteralPath $package -Recurse -Force }
  $bin = Join-Path $package 'bin'
  $libexec = Join-Path $package 'libexec'
  $share = Join-Path $package 'share'
  $licenses = Join-Path $package 'licenses'
  New-Item -ItemType Directory -Force -Path $bin, $libexec, $share, $licenses | Out-Null
  New-Item -ItemType Directory -Force -Path (Join-Path $share 'fixtures'), (Join-Path $share 'schemas'), (Join-Path $share 'workers') | Out-Null

  $env:CGO_ENABLED = '1'
  $env:GOOS = 'windows'
  $env:GOARCH = $arch
  foreach ($app in @('stable', 'agentctl', 'agentworker')) {
    $destination = if ($app -eq 'stable') { Join-Path $bin "$app.exe" } else { Join-Path $libexec "$app.exe" }
    & go build -p 2 -buildvcs=false -trimpath -ldflags "-X main.version=$version" -o $destination "./cmd/$app"
    if ($LASTEXITCODE -ne 0) { throw "go build failed for $app ($LASTEXITCODE)" }
  }

  $temporalRoot = Join-Path $scratch 'temporal'
  Expand-Archive -LiteralPath $archive -DestinationPath $temporalRoot
  $temporalExe = Get-ChildItem -LiteralPath $temporalRoot -Filter 'temporal.exe' -File -Recurse | Select-Object -First 1
  $temporalLicense = Get-ChildItem -LiteralPath $temporalRoot -Filter 'LICENSE' -File -Recurse | Select-Object -First 1
  if (-not $temporalExe -or -not $temporalLicense) { throw 'Temporal archive is missing temporal.exe or LICENSE' }
  Copy-Item -LiteralPath $temporalExe.FullName -Destination (Join-Path $libexec 'temporal.exe')
  Copy-Item -LiteralPath $temporalLicense.FullName -Destination (Join-Path $licenses 'temporal-LICENSE')
  Copy-Item -LiteralPath (Join-Path $root 'fixtures/sensor_board') -Destination (Join-Path $share 'fixtures') -Recurse
  Get-ChildItem -LiteralPath (Join-Path $share 'fixtures/sensor_board') -File -Filter '~*.lck' | Remove-Item -Force
  Copy-Item -LiteralPath (Join-Path $root 'schemas/next_action.schema.json'), (Join-Path $root 'schemas/criteria_proposal.schema.json') -Destination (Join-Path $share 'schemas') -Force
  Copy-Item -LiteralPath (Join-Path $root 'workers/kicad'), (Join-Path $root 'workers/computer') -Destination (Join-Path $share 'workers') -Recurse
  Get-ChildItem -LiteralPath (Join-Path $share 'workers/kicad') -Directory -Filter '__pycache__' -Recurse | Remove-Item -Recurse -Force
  Copy-Item -LiteralPath (Join-Path $root 'scripts/install_windows.ps1') -Destination (Join-Path $package 'install.ps1')
  Copy-Item -LiteralPath (Join-Path $root 'VERSION') -Destination (Join-Path $package 'VERSION')

  $zip = Join-Path $distRoot "$name.zip"
  $checksumFile = "$zip.sha256"
  if (Test-Path -LiteralPath $zip) { Remove-Item -LiteralPath $zip -Force }
  if (Test-Path -LiteralPath $checksumFile) { Remove-Item -LiteralPath $checksumFile -Force }
  Compress-Archive -Path (Join-Path $package '*') -DestinationPath $zip
  $archiveDigest = (Get-FileHash -Algorithm SHA256 -LiteralPath $zip).Hash.ToLowerInvariant()
  [IO.File]::WriteAllText($checksumFile, "$archiveDigest  $([IO.Path]::GetFileName($zip))`r`n", [Text.UTF8Encoding]::new($false))
  Write-Output "Created $zip"
}
finally {
  if (Test-Path -LiteralPath $scratch) { Remove-Item -LiteralPath $scratch -Recurse -Force }
}
