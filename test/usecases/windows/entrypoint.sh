#!/bin/sh
# Installs the shared public key, creates the initial IIS and service configuration (a site
# with the application of UC20 on the default page, a service on a legacy executable), starts
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
  "/uc20-shop": {"path": "/srv/win/wwwroot", "pool": "uc20-shop"}}},
 "pools": {"DefaultAppPool": {"state": "Started", "starts": 0, "recycles": 0},
  "uc20-shop": {"state": "Started", "starts": 0, "recycles": 0}}}
JSON
cat >/srv/win/state/services.json <<'JSON'
{"UC20Worker": {"path": "\"/srv/win/legacy/worker.exe\"", "state": "Running", "starts": 0}}
JSON
chown -R deploy:deploy /srv/win
ssh-keygen -A >/dev/null
/usr/sbin/sshd -e
exec su deploy -s /bin/sh -c "WINRM_PASSWORD='$(cat /keys/win-pass)' exec /usr/local/bin/manent-uc-windows"
