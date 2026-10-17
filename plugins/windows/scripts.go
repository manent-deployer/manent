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

// currentScript writes the path in service: physicalPath of the IIS application or PathName of
// the service. found=false if the site, the application or the service does not exist.
func currentScript(s settings) string {
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
// extracts it into a temporary directory and moves it to <version> with the sha256 marker.
// It is idempotent: a previous partial attempt is removed.
func uploadScript(s settings, version, sha string) string {
	return vars("root", s.ReleasesDir, "version", version, "sha", sha) + `
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
Expand-Archive -LiteralPath $zip -DestinationPath $tmp -Force
Set-Content -LiteralPath (Join-Path $tmp '.manent-sha256') -Value $sha
Move-Item -LiteralPath $tmp -Destination $dir
Remove-Item -LiteralPath $zip -Force
Write-Output ('MANENT:dir=' + $dir)
`
}

// activateScript puts the release in service. IIS: physicalPath of the application to the
// release directory, with the application pool stopped and restarted (stop_app_pool) or
// recycled. Service: stop, binPath to the release executable, start and wait for Running.
// Then post_script, with MANENT_VERSION and MANENT_RELEASE_DIR in the environment.
func activateScript(s settings, version string) string {
	head := vars("root", s.ReleasesDir, "version", version) + `$dir = Join-Path $root $version
if (-not (Test-Path -LiteralPath $dir)) { throw "release $version missing on the target ($dir)" }
`
	var body string
	if s.Mode == ModeIIS {
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
	} else {
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
