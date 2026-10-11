# VM con Docker per il plugin container (UC6): Docker in Docker più sshd, con l'utente deploy
# nel gruppo docker. Il registry del laboratorio è in chiaro (registry:5000).
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
