# Manifest of the Network Load Balancing emulation (use cases).
@{
  RootModule        = 'NetworkLoadBalancingClusters.psm1'
  ModuleVersion     = '1.0.0'
  GUID              = '0b3f5e2a-91c4-4f6e-a7d2-6e8b4c1f2a33'
  FunctionsToExport = @('Get-NlbClusterNode', 'Stop-NlbClusterNode', 'Start-NlbClusterNode')
}
