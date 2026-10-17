# State of the emulated Windows host (use cases): IIS configuration and services, in JSON files
# shared with manent-uc-windows, which serves the IIS applications.
$script:StateDir = if ($env:STATE_DIR) { $env:STATE_DIR } else { '/srv/win/state' }

function Read-ManentState([string] $Name) {
  Get-Content -Raw -LiteralPath (Join-Path $script:StateDir "$Name.json") | ConvertFrom-Json -AsHashtable
}

function Write-ManentState([string] $Name, $Value) {
  $path = Join-Path $script:StateDir "$Name.json"
  $tmp = "$path.tmp"
  $Value | ConvertTo-Json -Depth 8 | Set-Content -LiteralPath $tmp
  Move-Item -LiteralPath $tmp -Destination $path -Force
}
