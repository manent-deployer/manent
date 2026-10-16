# VM with Docker for the container plugin (UC6): Docker in Docker plus sshd, with the deploy
# user in the docker group. The lab registry is plain-text (registry:5000).
FROM docker:27.3.1-dind
RUN apk add --no-cache --upgrade openssh-server openssh-client \
 && adduser -D -s /bin/sh deploy \
 && addgroup deploy docker \
 && sed -i 's/^deploy:!/deploy:*/' /etc/shadow \
 && mkdir -p /home/deploy/.ssh \
 && chmod 700 /home/deploy/.ssh \
 && chown -R deploy:deploy /home/deploy \
 && printf 'PasswordAuthentication no\nPermitRootLogin no\n' >> /etc/ssh/sshd_config
COPY entrypoint.sh /vm-entrypoint.sh
EXPOSE 22 8080
ENTRYPOINT ["/vm-entrypoint.sh"]
