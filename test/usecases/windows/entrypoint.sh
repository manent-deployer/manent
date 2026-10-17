#!/bin/sh
# Installs the shared public key, creates the initial IIS, service, task, MSI, ARR and NLB state
# (the applications of UC20 and UC21 on the default page, a service and a task on a legacy
# executable), starts
# sshd and runs the WinRM endpoint and the IIS web server as the deploy user.
set -eu
until [ -s /keys/id.pub ] && [ -s /keys/win-pass ]; do sleep 1; done
cp /keys/id.pub /home/deploy/.ssh/authorized_keys
chown deploy:deploy /home/deploy/.ssh/authorized_keys
chmod 600 /home/deploy/.ssh/authorized_keys
echo "<h1>IIS</h1>" >/srv/win/wwwroot/index.html
printf '#!/bin/sh\nexit 0\n' >/srv/win/legacy/worker.exe
cat >/srv/win/state/iis.json <<'JSON'
{"sites": {"Default Web Site": {"/": {"path": "/srv/win/wwwroot", "pool": "DefaultAppPool"},
  "/uc20-shop": {"path": "/srv/win/wwwroot", "pool": "uc20-shop"},
  "/uc21-web": {"path": "/srv/win/wwwroot", "pool": "uc21-web"}}},
 "pools": {"DefaultAppPool": {"state": "Started", "starts": 0, "recycles": 0},
  "uc20-shop": {"state": "Started", "starts": 0, "recycles": 0},
  "uc21-web": {"state": "Started", "starts": 0, "recycles": 0}}}
JSON
# UC21: a scheduled task with a stored password, no MSI product installed, and (read on win1,
# the management host) an ARR web farm and an NLB cluster with the two Windows hosts.
cat >/srv/win/state/tasks.json <<'JSON'
{"\\UC21\\Export": {"execute": "/srv/win/legacy/worker.exe", "arguments": "", "dir": "/srv/win/legacy",
  "password": "uc21-job-pass", "running_until": 0, "runs": 0, "last_result": 0}}
JSON
echo '{}' >/srv/win/state/products.json
echo '{"farms": {"uc21-farm": {"win1": {"enabled": true, "state": 0, "healthy": true, "requests": 0},
  "win2": {"enabled": true, "state": 0, "healthy": true, "requests": 0}}}, "history": []}' >/srv/win/state/arr.json
echo '{"nodes": {"win1": "Converged", "win2": "Converged"}, "history": []}' >/srv/win/state/nlb.json
cat >/srv/win/state/services.json <<'JSON'
{"UC20Worker": {"path": "\"/srv/win/legacy/worker.exe\"", "state": "Running", "starts": 0}}
JSON
chown -R deploy:deploy /srv/win
ssh-keygen -A >/dev/null
/usr/sbin/sshd -e
# windir: where the iis-arr pool finds Microsoft.Web.Administration, as on Windows.
exec su deploy -s /bin/sh -c "windir=/srv/win/windows WINRM_PASSWORD='$(cat /keys/win-pass)' exec /usr/local/bin/manent-uc-windows"
