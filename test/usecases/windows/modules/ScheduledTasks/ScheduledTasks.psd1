# Manifest of the scheduled task emulation (use cases).
@{
  RootModule        = 'ScheduledTasks.psm1'
  ModuleVersion     = '1.0.0'
  GUID              = '5d9f1f7a-2b0e-4d8c-8f6c-3c1e7a2b9d10'
  FunctionsToExport = @('Get-ScheduledTask', 'Set-ScheduledTask', 'New-ScheduledTaskAction', 'Start-ScheduledTask', 'Stop-ScheduledTask')
}
