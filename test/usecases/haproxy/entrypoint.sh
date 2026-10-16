#!/bin/sh
# Starts HAProxy (master-worker) and then the Data Plane API, which reloads the master with
# SIGUSR2 after every configuration change.
set -eu
haproxy -W -db -S /var/run/haproxy/master.sock -f /usr/local/etc/haproxy/haproxy.cfg &
echo $! >/tmp/haproxy.pid
until [ -S /var/run/haproxy/api.sock ]; do sleep 0.2; done
exec /usr/local/bin/dataplaneapi -f /usr/local/etc/haproxy/dataplaneapi.yml
