# Manifest of the service emulation: the functions are loaded on first use, as the Windows
# cmdlets would be found without Import-Module.
@{
  RootModule        = 'ManentWindowsStub.psm1'
  ModuleVersion     = '1.0.0'
  GUID              = '8f6f3c2e-6a52-4e0c-9a2d-1f7c6c6a1e01'
  FunctionsToExport = @('Get-CimInstance', 'Get-Service', 'Stop-Service', 'Start-Service')
}
