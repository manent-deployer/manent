# Emulation of the Windows service cmdlets for the use cases (Get-CimInstance Win32_Service and
# Win32_InstalledWin32Program,
# Get-Service, Stop-Service, Start-Service), on services.json. Starting a service runs its
# executable with --check: an executable that fails makes the start fail, as a service that
# stops at startup on Windows.
. (Join-Path $PSScriptRoot 'State.ps1')

function Get-ServiceEntry([hashtable] $s, [string] $Name) {
  if (-not $s.ContainsKey($Name)) { throw "Cannot find any service with service name '$Name'." }
  $s[$Name]
}

function Get-CimInstance {
  param([string] $ClassName, [string] $Filter)
  if ($ClassName -eq 'Win32_InstalledWin32Program') {
    # Installed MSI products, from products.json (written by the msiexec.exe emulation).
    if ($Filter -notmatch "^(Name|MsiProductCode)='([^']+)'$") { throw "query non supportata: $ClassName $Filter" }
    $field, $value = $Matches[1], $Matches[2]
    $p = Read-ManentState 'products'
    foreach ($code in $p.Keys) {
      $e = $p[$code]
      if (($field -eq 'Name' -and $e.name -eq $value) -or ($field -eq 'MsiProductCode' -and $code -eq $value)) {
        [pscustomobject]@{ Name = $e.name; Version = $e.version; MsiProductCode = $code }
      }
    }
    return
  }
  if ($ClassName -ne 'Win32_Service' -or $Filter -notmatch "Name='([^']+)'") { throw "query non supportata: $ClassName $Filter" }
  $name = $Matches[1]
  $s = Read-ManentState 'services'
  if (-not $s.ContainsKey($name)) { return $null }
  [pscustomobject]@{ Name = $name; PathName = $s[$name].path; State = $s[$name].state }
}

function Get-Service {
  param([string] $Name)
  $s = Read-ManentState 'services'
  $e = Get-ServiceEntry $s $Name
  $svc = [pscustomobject]@{ Name = $Name; Status = $e.state }
  $svc | Add-Member -MemberType ScriptMethod -Name WaitForStatus -Value {
    param($status, $timeout)
    $deadline = (Get-Date) + $timeout
    while ((Get-ServiceEntry (Read-ManentState 'services') $this.Name).state -ne $status) {
      if ((Get-Date) -gt $deadline) { throw "Time out has expired and the operation has not been completed." }
      Start-Sleep -Milliseconds 200
    }
  }
  $svc
}

function Stop-Service {
  param([string] $Name, [switch] $Force)
  $s = Read-ManentState 'services'
  (Get-ServiceEntry $s $Name).state = 'Stopped'
  Write-ManentState 'services' $s
}

function Start-Service {
  param([string] $Name)
  $s = Read-ManentState 'services'
  $e = Get-ServiceEntry $s $Name
  $exe = $e.path
  if ($exe.StartsWith('"')) { $exe = $exe.Substring(1, $exe.IndexOf('"', 1) - 1) } else { $exe = ($exe -split ' ')[0] }
  & /bin/sh $exe --check *> $null
  if ($LASTEXITCODE -ne 0) { throw "Service '$Name ($Name)' cannot be started due to the following error: Cannot start service $Name on computer '.'." }
  $e.state = 'Running'
  $e.starts = [int] $e.starts + 1
  Write-ManentState 'services' $s
}

Export-ModuleMember -Function Get-CimInstance, Get-Service, Stop-Service, Start-Service
