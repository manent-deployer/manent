#!/bin/sh
# Starts the demo: five hosts emulated on loopback addresses (one SSH user and one HTTP server
# each), the identity provider of the web interface, then manent-server (API, web interface and
# worker) and the history seed.
set -eu
HOSTS="staging-1=127.0.1.10 web-eu-1a=127.0.1.1 web-eu-1b=127.0.1.2 web-us-1a=127.0.1.3 web-us-1b=127.0.1.4"

home=/var/lib/manent
mkdir -p $home/keys $home/.ssh
[ -f $home/keys/id ] || ssh-keygen -q -t ed25519 -N '' -C manent-demo -f $home/keys/id
# One host key, ed25519, created at the first start: it is the only one in known_hosts.
[ -f /etc/ssh/ssh_host_ed25519_key ] || ssh-keygen -q -t ed25519 -N '' -f /etc/ssh/ssh_host_ed25519_key
hostkey=$(cut -d' ' -f1,2 /etc/ssh/ssh_host_ed25519_key.pub)
: >$home/.ssh/known_hosts
for h in $HOSTS; do
  name=${h%%=*} ip=${h#*=}
  grep -q " $name\$" /etc/hosts || echo "$ip $name" >>/etc/hosts
  install -d -o "$name" -g "$name" -m 700 "/home/$name/.ssh"
  install -o "$name" -g "$name" -m 600 $home/keys/id.pub "/home/$name/.ssh/authorized_keys"
  install -d -o "$name" -g "$name" "/home/$name/apps"
  echo "$name $hostkey" >>$home/.ssh/known_hosts
  # The host's applications: http://<host>:8081/<app>/current/
  setpriv --reuid "$name" --regid "$name" --init-groups busybox httpd -p "$ip:8081" -h "/home/$name/apps"
done
chown -R manent:manent $home
/usr/sbin/sshd

# The web interface signs in here as John Doe. It must answer before serve, which reads its
# discovery document at startup; the issuer is the address the browser uses (port 9000 published).
setpriv --reuid manent --regid manent --init-groups manent-demo-idp -listen :9000 -issuer http://localhost:9000 \
  -user john.doe:admins -user-name "John Doe" &
until busybox wget -q -O /dev/null http://127.0.0.1:9000/healthz; do sleep 0.2; done

if [ "${MANENT_DEMO_SEED:-true}" = true ]; then
  setpriv --reuid manent --regid manent --init-groups env HOME=$home /demo/seed.sh &
fi
cd $home
exec setpriv --reuid manent --regid manent --init-groups env HOME=$home manent-server serve
