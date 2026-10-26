# manent demo: server, web interface, worker and five emulated hosts in one container.
#
#   docker build -f demo/Dockerfile -t manent-demo .
#   docker run --rm -p 8080:8080 -p 9000:9000 --name manent-demo manent-demo
#
# Then open http://localhost:8080 (signed in as John Doe) and run the client inside the container:
#
#   docker exec -it manent-demo manent deploy -f /demo/deploys/shop-1.3.0.yml
#
# BINARIES=prebuilt skips the Go and web build and copies linux binaries already built in
# demo/.bin (manent and manent-server, with the web interface embedded, and manent-demo-idp).
ARG BINARIES=source

FROM node:22.11.0-bookworm-slim AS web
WORKDIR /src/web
COPY web/package.json web/package-lock.json ./
RUN npm ci --no-audit --no-fund
COPY api /src/api
COPY web ./
RUN npm run build

FROM golang:1.27 AS build-source
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=web /src/internal/webui/dist internal/webui/dist
ARG VERSION=demo
RUN LDFLAGS="-s -w -X github.com/manent-deployer/manent/internal/version.Version=${VERSION}" \
 && CGO_ENABLED=0 go build -trimpath -ldflags "$LDFLAGS" -o /out/manent ./cmd/manent \
 && CGO_ENABLED=0 go build -trimpath -ldflags "$LDFLAGS" -o /out/manent-server ./cmd/manent-server \
 && CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/manent-demo-idp ./test/usecases/oidc

FROM scratch AS build-prebuilt
COPY demo/.bin/manent demo/.bin/manent-server demo/.bin/manent-demo-idp /out/

FROM build-${BINARIES} AS build

# Debian and not distroless: the hosts need sshd, a shell and GNU coreutils (the remote-copy
# plugin uses sha256sum, mv -T and readlink), busybox httpd serves their applications.
FROM debian:bookworm-slim
RUN apt-get update \
 && apt-get install -y --no-install-recommends openssh-server openssh-client busybox tini ca-certificates \
 && rm -rf /var/lib/apt/lists/* \
 && mkdir -p /run/sshd \
 && rm -f /etc/ssh/ssh_host_* \
 && printf 'PasswordAuthentication no\nPermitRootLogin no\nHostKey /etc/ssh/ssh_host_ed25519_key\n' >/etc/ssh/sshd_config.d/demo.conf \
 && useradd --system --home-dir /var/lib/manent --create-home manent \
 && for h in staging-1 web-eu-1a web-eu-1b web-us-1a web-us-1b; do useradd -m -s /bin/sh "$h"; done
COPY --from=build /out/manent /out/manent-server /out/manent-demo-idp /usr/local/bin/
COPY demo/manent.yml /etc/manent/manent.yml
COPY demo/envconfig /demo/envconfig
COPY demo/deploys /demo/deploys
COPY demo/artifacts.sh demo/seed.sh demo/entrypoint.sh /demo/
RUN /demo/artifacts.sh /demo/artifacts
# The demo user's static token (its sha256 is in manent.yml): docker exec ... manent runs as John Doe.
ENV MANENT_CONFIG=/etc/manent/manent.yml MANENT_TOKEN=manent-demo-john-doe
EXPOSE 8080 9000
ENTRYPOINT ["tini", "--", "/demo/entrypoint.sh"]
