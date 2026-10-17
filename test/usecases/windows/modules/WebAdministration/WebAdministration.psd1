@{
  RootModule        = 'WebAdministration.psm1'
  ModuleVersion     = '1.0.0'
  GUID              = '5d1d0e43-2f33-4b6a-8b77-6c0a2f9d3e02'
  FunctionsToExport = @('Get-WebConfigurationProperty', 'Set-WebConfigurationProperty', 'Get-WebAppPoolState', 'Stop-WebAppPool', 'Start-WebAppPool', 'Restart-WebAppPool')
}
