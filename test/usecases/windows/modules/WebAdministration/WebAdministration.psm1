# Emulation of the IIS WebAdministration module for the use cases: the cmdlets used by the
# windows plugin, on the configuration kept in iis.json (sites, applications, application
# pools). Same parameters and results as on Windows Server.
. (Join-Path $PSScriptRoot '../ManentWindowsStub/State.ps1')

function Get-AppFromFilter([string] $Filter) {
  if ($Filter -notmatch "site\[@name='([^']+)'\]/application\[@path='([^']+)'\]") { throw "filtro non supportato: $Filter" }
  return @($Matches[1], $Matches[2])
}

function Get-WebConfigurationProperty {
  param([string] $PSPath, [string] $Filter, [string] $Name)
  $site, $app = Get-AppFromFilter $Filter
  $s = Read-ManentState 'iis'
  if (-not $s.sites.ContainsKey($site) -or -not $s.sites[$site].ContainsKey($app)) { return $null }
  # On Windows the result is a ConfigurationAttribute: the plugin reads .Value.
  [pscustomobject]@{ Name = $Name; Value = $s.sites[$site][$app].path }
}

function Set-WebConfigurationProperty {
  param([string] $PSPath, [string] $Filter, [string] $Name, $Value)
  $site, $app = Get-AppFromFilter $Filter
  $s = Read-ManentState 'iis'
  if (-not $s.sites.ContainsKey($site) -or -not $s.sites[$site].ContainsKey($app)) { throw "Filename: \\?\C:\Windows\system32\inetsrv\config\applicationHost.config Error: Cannot commit configuration changes" }
  $s.sites[$site][$app].path = [string] $Value
  Write-ManentState 'iis' $s
}

function Get-Pool([hashtable] $s, [string] $Name) {
  if (-not $s.pools.ContainsKey($Name)) { throw "Cannot find path 'IIS:\AppPools\$Name' because it does not exist." }
  $s.pools[$Name]
}

function Get-WebAppPoolState {
  param([string] $Name)
  $s = Read-ManentState 'iis'
  [pscustomobject]@{ Value = (Get-Pool $s $Name).state }
}

function Stop-WebAppPool {
  param([string] $Name)
  $s = Read-ManentState 'iis'
  (Get-Pool $s $Name).state = 'Stopped'
  Write-ManentState 'iis' $s
}

function Start-WebAppPool {
  param([string] $Name)
  $s = Read-ManentState 'iis'
  $p = Get-Pool $s $Name
  $p.state = 'Started'
  $p.starts = [int] $p.starts + 1
  Write-ManentState 'iis' $s
}

function Restart-WebAppPool {
  param([string] $Name)
  $s = Read-ManentState 'iis'
  $p = Get-Pool $s $Name
  $p.state = 'Started'
  $p.recycles = [int] $p.recycles + 1
  Write-ManentState 'iis' $s
}

Export-ModuleMember -Function Get-WebConfigurationProperty, Set-WebConfigurationProperty, Get-WebAppPoolState, Stop-WebAppPool, Start-WebAppPool, Restart-WebAppPool
