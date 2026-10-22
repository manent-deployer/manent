# Git server of the use case lab (UC27): git http-backend behind manent-uc-git, built by run.sh in
# .work/bin; ssh-keygen signs the commits of the case.
FROM alpine:3.20
RUN apk add --no-cache git git-daemon openssh-keygen
COPY manent-uc-git /usr/local/bin/
ENTRYPOINT ["/usr/local/bin/manent-uc-git"]
