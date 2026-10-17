# Emulation of the NLB cmdlets for the use cases, on nlb.json: the nodes with their state and the
# history of the changes ("<node>:<state>"), which the use case reads to check the drain order.
. (Join-Path $PSScriptRoot '../ManentWindowsStub/State.ps1')

function Get-NlbClusterNode {
  $s = Read-ManentState 'nlb'
  foreach ($n in $s.nodes.Keys | Sort-Object) { [pscustomobject]@{ Name = $n; State = $s.nodes[$n] } }
}

function Set-NodeState($HostName, $State) {
  $s = Read-ManentState 'nlb'
  $k = @($s.nodes.Keys | Where-Object { $_ -eq $HostName })
  if ($k.Count -eq 0) { throw "Cluster node $HostName not found" }
  $s.nodes[$k[0]] = $State
  $s.history = @($s.history) + "$($k[0]):$State"
  Write-ManentState 'nlb' $s
}

function Stop-NlbClusterNode {
  param([string] $HostName, [switch] $Drain, [uint32] $Timeout)
  if ($Drain) { Set-NodeState $HostName 'Draining'; Start-Sleep -Seconds 1 }
  Set-NodeState $HostName 'Stopped'
}

function Start-NlbClusterNode {
  param([string] $HostName)
  Set-NodeState $HostName 'Converged'
}

Export-ModuleMember -Function Get-NlbClusterNode, Stop-NlbClusterNode, Start-NlbClusterNode
