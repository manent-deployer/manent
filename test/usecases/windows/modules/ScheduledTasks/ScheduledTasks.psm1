# Emulation of the ScheduledTasks module for the use cases, on tasks.json (key: path + name).
# A task is Running until its running_until (Unix seconds); starting it runs the executable
# with --check and counts the runs.
. (Join-Path $PSScriptRoot '../ManentWindowsStub/State.ps1')

function Get-TaskEntry($s, $TaskPath, $TaskName) {
  $k = $TaskPath + $TaskName
  if (-not $s.ContainsKey($k)) { return $null }
  $s[$k]
}

function Get-ScheduledTask {
  [CmdletBinding()]
  param([string] $TaskName, [string] $TaskPath = '\')
  $e = Get-TaskEntry (Read-ManentState 'tasks') $TaskPath $TaskName
  if ($null -eq $e) { Write-Error "No MSFT_ScheduledTask objects found with property 'TaskName' equal to '$TaskName'."; return }
  $state = 'Ready'
  if ([long]$e.running_until -gt [DateTimeOffset]::UtcNow.ToUnixTimeSeconds()) { $state = 'Running' }
  [pscustomobject]@{ TaskName = $TaskName; TaskPath = $TaskPath; State = $state
    Actions = @([pscustomobject]@{ Execute = $e.execute; Arguments = $e.arguments; WorkingDirectory = $e.dir }) }
}

function New-ScheduledTaskAction {
  param([string] $Execute, [string] $Argument, [string] $WorkingDirectory)
  [pscustomobject]@{ Execute = $Execute; Arguments = $Argument; WorkingDirectory = $WorkingDirectory }
}

function Set-ScheduledTask {
  param([string] $TaskName, [string] $TaskPath = '\', $Action, [string] $User, [string] $Password)
  $s = Read-ManentState 'tasks'
  $e = Get-TaskEntry $s $TaskPath $TaskName
  if ($null -eq $e) { throw "The system cannot find the file specified." }
  if ($User -ne '' -and $Password -ne $e.password) { throw "The user name or password is incorrect." }
  $e.execute, $e.arguments, $e.dir = $Action.Execute, $Action.Arguments, $Action.WorkingDirectory
  Write-ManentState 'tasks' $s
  Get-ScheduledTask -TaskName $TaskName -TaskPath $TaskPath
}

function Stop-ScheduledTask {
  param([string] $TaskName, [string] $TaskPath = '\')
  $s = Read-ManentState 'tasks'
  (Get-TaskEntry $s $TaskPath $TaskName).running_until = 0
  Write-ManentState 'tasks' $s
}

function Start-ScheduledTask {
  param([string] $TaskName, [string] $TaskPath = '\')
  $s = Read-ManentState 'tasks'
  $e = Get-TaskEntry $s $TaskPath $TaskName
  & /bin/sh $e.execute --check *> $null
  $e.last_result = $LASTEXITCODE
  $e.runs = [int] $e.runs + 1
  Write-ManentState 'tasks' $s
}

Export-ModuleMember -Function Get-ScheduledTask, Set-ScheduledTask, New-ScheduledTaskAction, Start-ScheduledTask, Stop-ScheduledTask
