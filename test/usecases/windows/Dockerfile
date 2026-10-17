# Use case target emulating a Windows host for the windows plugin: a real PowerShell (7, on
# Linux), OpenSSH, the WinRM endpoint and the IIS web server of manent-uc-windows (mounted by
# compose from .work/bin), and PowerShell modules with the IIS and service cmdlets the plugin
# uses, on a JSON state. What runs is the plugin's PowerShell, not an imitation of it.
FROM mcr.microsoft.com/powershell:lts-ubuntu-22.04
RUN apt-get update \
 && apt-get install -y --no-install-recommends openssh-server curl \
 && rm -rf /var/lib/apt/lists/* \
 && useradd -m -s /bin/bash deploy \
 && mkdir -p /run/sshd /home/deploy/.ssh /srv/win/state /srv/win/apps /srv/win/wwwroot /srv/win/legacy \
 && chmod 700 /home/deploy/.ssh && chown -R deploy:deploy /home/deploy \
 && printf 'PasswordAuthentication no\nPermitRootLogin no\n' > /etc/ssh/sshd_config.d/manent.conf
COPY modules/ /usr/local/share/powershell/Modules/
COPY sc.exe /usr/local/bin/sc.exe
COPY entrypoint.sh /entrypoint.sh
EXPOSE 22 5986 8080
ENTRYPOINT ["/entrypoint.sh"]
