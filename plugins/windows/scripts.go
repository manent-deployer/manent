package windows

import (
	"fmt"
	"strings"
)

// The scripts run on Windows PowerShell 5.1 and on PowerShell 7: no syntax or cmdlet of only
// one of the two. Results go on stdout as MANENT:<key>=<value> lines; a terminating error
// fails the script (bootstrap).

// vars declares the script variables with their values as PowerShell literals.
func vars(kv ...string) string {
	var b strings.Builder
	for i := 0; i+1 < len(kv); i += 2 {
		fmt.Fprintf(&b, "$%s = %s\n", kv[i], psQuote(kv[i+1]))
	}
	return b.String()
}

// currentScript writes the path in service: physicalPath of the IIS application, PathName of
// the service, executable of the scheduled task or release directory of the installed MSI
// product. found=false if the site, the application, the service or the task does not exist.
func currentScript(s settings) string {
	switch s.Mode {
	case ModeTask:
		return vars("name", s.Task, "tpath", s.TaskPath) + `
$t = Get-ScheduledTask -TaskName $name -TaskPath $tpath -ErrorAction SilentlyContinue
if ($null -eq $t) { Write-Output 'MANENT:found=false'; return }
Write-Output 'MANENT:found=true'
Write-Output ('MANENT:path=' + @($t.Actions)[0].Execute)
Write-Output ('MANENT:state=' + $t.State)
`
	case ModeMSI:
		// The release is the one recorded at installation, as long as its product is still
		// installed with the same version: a product removed or replaced by hand is no release.
		return vars("root", s.ReleasesDir) + msiProduct + `
Write-Output 'MANENT:found=true'
$cur = Join-Path $root '.manent-current'
if (-not (Test-Path -LiteralPath $cur)) { return }
$v = (Get-Content -LiteralPath $cur -Raw).Trim()
$meta = Join-Path (Join-Path $root $v) '.manent-product'
if (-not (Test-Path -LiteralPath $meta)) { return }
$code, $pv = @(Get-Content -LiteralPath $meta)
$p = Get-MsiProduct "MsiProductCode='$code'"
if ($null -ne $p -and $p.Version -eq $pv) { Write-Output ('MANENT:path=' + (Join-Path $root $v)) }
`
	}
	if s.Mode == ModeIIS {
		return vars("site", s.Site, "app", s.App) + iisFilter + `
$v = Get-WebConfigurationProperty -PSPath 'MACHINE/WEBROOT/APPHOST' -Filter $filter -Name physicalPath
if ($null -eq $v) { Write-Output 'MANENT:found=false'; return }
if ($v -isnot [string]) { $v = $v.Value }
Write-Output 'MANENT:found=true'
Write-Output ('MANENT:path=' + $v)
`
	}
	return vars("name", s.Service) + `
$svc = Get-CimInstance -ClassName Win32_Service -Filter "Name='$name'"
if ($null -eq $svc) { Write-Output 'MANENT:found=false'; return }
Write-Output 'MANENT:found=true'
Write-Output ('MANENT:path=' + $svc.PathName)
Write-Output ('MANENT:state=' + $svc.State)
`
}

// msiProduct defines Get-MsiProduct: the installed products matching a WQL filter, from
// Win32_InstalledWin32Program (unlike Win32_Product, reading it does not start a repair of the
// installed packages). A product code is checked before it enters a filter.
const msiProduct = `function Get-MsiProduct($filter) {
  if ($filter -match "MsiProductCode='(.*)'" -and $Matches[1] -notmatch '^\{[0-9A-Fa-f-]{36}\}$') { return $null }
  @(Get-CimInstance -ClassName Win32_InstalledWin32Program -Filter $filter) | Select-Object -First 1
}
`

// iisFilter selects the root virtual directory of the application (path "/" is the site).
const iisFilter = `Import-Module WebAdministration
$filter = "system.applicationHost/sites/site[@name='$site']/application[@path='$app']/virtualDirectory[@path='/']"
`

// probeScript checks PowerShell, the releases directory (created if missing, writable) and
// whether the release is already extracted with the same sha256 (present=true).
func probeScript(s settings, version, sha string) string {
	return vars("root", s.ReleasesDir, "version", version, "sha", sha) + `
Write-Output ('MANENT:powershell=' + $PSVersionTable.PSVersion.ToString())
New-Item -ItemType Directory -Force -Path $root | Out-Null
$probe = Join-Path $root ('.manent-probe-' + [guid]::NewGuid().ToString('N'))
Set-Content -LiteralPath $probe -Value 'ok'
Remove-Item -LiteralPath $probe -Force
$dir = Join-Path $root $version
$marker = Join-Path $dir '.manent-sha256'
$present = 'false'
if ((Test-Path -LiteralPath $marker) -and ((Get-Content -LiteralPath $marker -Raw).Trim() -eq $sha)) { $present = 'true' }
Write-Output ('MANENT:present=' + $present)
`
}

// uploadScript writes the data received on stdin to <version>.zip.part, checks its sha256,
// extracts it into a temporary directory (an MSI package is kept as package.msi) and moves it
// to <version> with the sha256 marker. It is idempotent: a previous partial attempt is removed.
func uploadScript(s settings, version, sha string) string {
	return vars("root", s.ReleasesDir, "version", version, "sha", sha, "msi", fmt.Sprint(s.Mode == ModeMSI)) + `
$zip = Join-Path $root ($version + '.zip')
$part = $zip + '.part'
$fs = [IO.File]::Create($part)
try {
  while ($true) {
    $l = [Console]::In.ReadLine()
    if ($null -eq $l -or $l -eq '') { break }
    $b = [Convert]::FromBase64String($l)
    $fs.Write($b, 0, $b.Length)
  }
} finally { $fs.Close() }
$got = (Get-FileHash -Algorithm SHA256 -LiteralPath $part).Hash.ToLowerInvariant()
if ($got -ne $sha) { Remove-Item -LiteralPath $part -Force; throw "sha256 on the target $got, expected $sha" }
Move-Item -LiteralPath $part -Destination $zip -Force
$dir = Join-Path $root $version
$tmp = $dir + '.tmp'
if (Test-Path -LiteralPath $tmp) { Remove-Item -LiteralPath $tmp -Recurse -Force }
if (Test-Path -LiteralPath $dir) { Remove-Item -LiteralPath $dir -Recurse -Force }
if ($msi -eq 'True') {
  New-Item -ItemType Directory -Force -Path $tmp | Out-Null
  Move-Item -LiteralPath $zip -Destination (Join-Path $tmp 'package.msi')
} else {
  Expand-Archive -LiteralPath $zip -DestinationPath $tmp -Force
  Remove-Item -LiteralPath $zip -Force
}
Set-Content -LiteralPath (Join-Path $tmp '.manent-sha256') -Value $sha
Move-Item -LiteralPath $tmp -Destination $dir
Write-Output ('MANENT:dir=' + $dir)
`
}

// activateScript puts the release in service. IIS: physicalPath of the application to the
// release directory, with the application pool stopped and restarted (stop_app_pool) or
// recycled. Service: stop, binPath to the release executable, start and wait for Running.
// Task: wait for (or stop) the running instance, action to the release executable. MSI:
// install the package, after uninstalling the installed product with uninstall_previous, on
// rollback or when going back to a release installed before (packages usually refuse
// downgrades). Then post_script, with MANENT_VERSION and
// MANENT_RELEASE_DIR in the environment. password is the task account's, resolved by the caller.
func activateScript(s settings, version string, rollback bool, password string) string {
	head := vars("root", s.ReleasesDir, "version", version) + `$dir = Join-Path $root $version
if (-not (Test-Path -LiteralPath $dir)) { throw "release $version missing on the target ($dir)" }
`
	var body string
	switch s.Mode {
	case ModeTask:
		body = vars("name", s.Task, "tpath", s.TaskPath, "binary", s.Binary, "arguments", s.Arguments, "timeout", seconds(s.Timeout),
			"stop", fmt.Sprint(s.StopTask), "run", fmt.Sprint(s.RunNow), "user", s.TaskUser, "password", password) + `
$exe = Join-Path $dir $binary
if (-not (Test-Path -LiteralPath $exe)) { throw "executable $binary missing in release $version" }
$t = Get-ScheduledTask -TaskName $name -TaskPath $tpath -ErrorAction SilentlyContinue
if ($null -eq $t) { throw "scheduled task $tpath$name not found" }
if ($t.State -eq 'Running' -and $stop -eq 'True') { Stop-ScheduledTask -TaskName $name -TaskPath $tpath }
$deadline = (Get-Date).AddSeconds([int]$timeout)
while ((Get-ScheduledTask -TaskName $name -TaskPath $tpath).State -eq 'Running') {
  if ((Get-Date) -gt $deadline) { throw "scheduled task $tpath$name still running after $timeout s (stop_task: true to stop it)" }
  Start-Sleep -Seconds 1
}
if ($arguments -ne '') { $a = New-ScheduledTaskAction -Execute $exe -Argument $arguments -WorkingDirectory $dir }
else { $a = New-ScheduledTaskAction -Execute $exe -WorkingDirectory $dir }
if ($user -ne '') { Set-ScheduledTask -TaskName $name -TaskPath $tpath -Action $a -User $user -Password $password | Out-Null }
else { Set-ScheduledTask -TaskName $name -TaskPath $tpath -Action $a | Out-Null }
if ($run -eq 'True') { Start-ScheduledTask -TaskName $name -TaskPath $tpath }
`
	case ModeMSI:
		props := make([]string, len(s.MSIProps))
		for i, p := range s.MSIProps {
			props[i] = psQuote(p)
		}
		body = vars("product", s.Product, "uninstall", fmt.Sprint(s.Uninstall || rollback)) + fmt.Sprintf("$props = @(%s)\n", strings.Join(props, ", ")) + msiProduct + `
$msi = Join-Path $dir 'package.msi'
if (-not (Test-Path -LiteralPath $msi)) { throw "package.msi missing in release $version" }
$cur = Join-Path $root '.manent-current'
$meta = Join-Path $dir '.manent-product'
function Invoke-Msiexec($argv, $what) {
  $p = Start-Process -FilePath 'msiexec.exe' -ArgumentList $argv -Wait -PassThru
  if (@(0, 1605, 3010) -notcontains $p.ExitCode) { throw "msiexec ${what}: exit $($p.ExitCode) (log in $dir)" }
  if ($p.ExitCode -eq 3010) { Write-Output 'MANENT:reboot=required' }
}
$installed = Get-MsiProduct "Name='$product'"
# A release installed before (meta present) whose product is not the installed one is a
# downgrade, e.g. a manual rollback: packages refuse it, so the installed product goes first.
$code, $pv = $null, $null
if (Test-Path -LiteralPath $meta) { $code, $pv = @(Get-Content -LiteralPath $meta) }
$done = ($null -ne $installed -and (Test-Path -LiteralPath $cur) -and (Get-Content -LiteralPath $cur -Raw).Trim() -eq $version -and
  $installed.MsiProductCode -eq $code -and $installed.Version -eq $pv)
if (-not $done) {
  if ($null -ne $installed -and ($uninstall -eq 'True' -or ($null -ne $code -and $installed.MsiProductCode -ne $code))) {
    Invoke-Msiexec @('/x', $installed.MsiProductCode, '/qn', '/norestart', '/l*v', ('"' + (Join-Path $dir 'uninstall.log') + '"')) 'uninstall'
  }
  Invoke-Msiexec (@('/i', ('"' + $msi + '"'), '/qn', '/norestart', '/l*v', ('"' + (Join-Path $dir 'install.log') + '"')) + $props) 'install'
  $installed = Get-MsiProduct "Name='$product'"
  if ($null -eq $installed) { throw "product $product not installed after msiexec (log in $dir)" }
  Set-Content -LiteralPath $meta -Value @($installed.MsiProductCode, $installed.Version)
  Set-Content -LiteralPath $cur -Value $version
}
Write-Output ('MANENT:product=' + $installed.MsiProductCode + ' ' + $installed.Version)
`
	case ModeIIS:
		body = vars("site", s.Site, "app", s.App, "pool", s.AppPool, "stop", fmt.Sprint(s.StopAppPool), "timeout", seconds(s.Timeout)) + iisFilter + `
if ($null -eq (Get-WebConfigurationProperty -PSPath 'MACHINE/WEBROOT/APPHOST' -Filter $filter -Name physicalPath)) { throw "application $app of site $site not found" }
function Wait-Pool($state) {
  $deadline = (Get-Date).AddSeconds([int]$timeout)
  while ((Get-WebAppPoolState -Name $pool).Value -ne $state) {
    if ((Get-Date) -gt $deadline) { throw "application pool $pool not $state within $timeout s" }
    Start-Sleep -Milliseconds 500
  }
}
if ($pool -ne '' -and $stop -eq 'True' -and (Get-WebAppPoolState -Name $pool).Value -ne 'Stopped') { Stop-WebAppPool -Name $pool; Wait-Pool 'Stopped' }
Set-WebConfigurationProperty -PSPath 'MACHINE/WEBROOT/APPHOST' -Filter $filter -Name physicalPath -Value $dir
if ($pool -ne '') {
  if ((Get-WebAppPoolState -Name $pool).Value -eq 'Stopped') { Start-WebAppPool -Name $pool } else { Restart-WebAppPool -Name $pool }
  Wait-Pool 'Started'
}
`
	default:
		body = vars("name", s.Service, "binary", s.Binary, "arguments", s.Arguments, "timeout", seconds(s.Timeout)) + `
$svc = Get-Service -Name $name
$exe = Join-Path $dir $binary
if (-not (Test-Path -LiteralPath $exe)) { throw "executable $binary missing in release $version" }
$wait = [TimeSpan]::FromSeconds([int]$timeout)
if ($svc.Status -ne 'Stopped') { Stop-Service -Name $name -Force; $svc.WaitForStatus('Stopped', $wait) }
$bin = '"' + $exe + '"'
if ($arguments -ne '') { $bin = $bin + ' ' + $arguments }
$out = & sc.exe config $name binPath= $bin
if ($LASTEXITCODE -ne 0) { throw "sc.exe config: $out" }
Start-Service -Name $name
(Get-Service -Name $name).WaitForStatus('Running', $wait)
`
	}
	post := ""
	if s.PostScript != "" {
		post = "$env:MANENT_VERSION = $version\n$env:MANENT_RELEASE_DIR = $dir\n& {\n" + s.PostScript + "\n}\n"
	}
	return head + body + post + "Write-Output ('MANENT:active=' + $version)\n"
}

// cleanupScript removes the releases beyond keep (by creation, most recent first), never
// the ones listed in protect (the release in service and the previous one).
func cleanupScript(s settings, keep int, protect ...string) string {
	quoted := make([]string, len(protect))
	for i, p := range protect {
		quoted[i] = psQuote(p)
	}
	return vars("root", s.ReleasesDir) + fmt.Sprintf("$keep = %d\n$protect = @(%s)\n", keep, strings.Join(quoted, ", ")) + `
$dirs = Get-ChildItem -LiteralPath $root -Directory | Where-Object { -not $_.Name.EndsWith('.tmp') } | Sort-Object CreationTimeUtc -Descending
$i = 0
foreach ($d in $dirs) {
  $i++
  if ($i -le $keep -or $protect -contains $d.Name) { continue }
  Remove-Item -LiteralPath $d.FullName -Recurse -Force
  Write-Output ('MANENT:removed=' + $d.Name)
}
`
}
