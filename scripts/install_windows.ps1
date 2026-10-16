param()

$ErrorActionPreference = 'Stop'
if ([Environment]::OSVersion.Platform -ne [PlatformID]::Win32NT) { throw 'Windows required' }
$source = (Resolve-Path $PSScriptRoot).Path
$localAppData = $env:LOCALAPPDATA
if ([string]::IsNullOrWhiteSpace($localAppData)) { throw 'LOCALAPPDATA is not set' }
$version = (Get-Content (Join-Path $source 'VERSION') -Raw).Trim()
if ($version -notmatch '^[0-9A-Za-z][0-9A-Za-z._+-]*$') { throw 'Invalid package VERSION' }
foreach ($relative in @('bin/stable.exe', 'libexec/agentctl.exe', 'libexec/agentworker.exe', 'libexec/temporal.exe', 'share/schemas/next_action.schema.json', 'share/schemas/criteria_proposal.schema.json')) {
  $path = Join-Path $source $relative
  if (-not (Test-Path -LiteralPath $path -PathType Leaf)) { throw "Package is incomplete: $relative" }
}

$programs = Join-Path $localAppData 'Programs/Stable'
$versions = Join-Path $programs 'versions'
$target = Join-Path $versions $version
foreach ($path in @($programs, $versions, $target)) {
  if (Test-Path -LiteralPath $path) {
    $item = Get-Item -LiteralPath $path -Force
    if ($item.Attributes -band [IO.FileAttributes]::ReparsePoint) { throw "Refusing reparse-point install path: $path" }
  }
}
New-Item -ItemType Directory -Force -Path $versions | Out-Null
if (Test-Path -LiteralPath $target) {
  if ((Get-Content (Join-Path $target 'VERSION') -Raw).Trim() -ne $version -or -not (Test-Path (Join-Path $target 'bin/stable.exe'))) {
    throw "Version path exists but is not a Stable installation: $target"
  }
} else {
  $stage = Join-Path $versions ('.install-' + [Guid]::NewGuid().ToString('N'))
  try {
    New-Item -ItemType Directory -Path $stage | Out-Null
    Copy-Item -Path (Join-Path $source '*') -Destination $stage -Recurse -Force
    Move-Item -LiteralPath $stage -Destination $target
  } finally {
    if (Test-Path -LiteralPath $stage) { Remove-Item -LiteralPath $stage -Recurse -Force }
  }
}
Write-Output "Installed $target"
Write-Output "Run: & `"$(Join-Path $target 'bin/stable.exe')`" doctor"
Write-Output 'User configuration and runtime data are stored outside the versioned program directory and are preserved by upgrades.'
