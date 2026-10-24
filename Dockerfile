# manent images, static (SQLite is pure Go) and without a shell, run as a non-root user:
#   - server (default): manent-server (API, worker, admin), the manent client, the external
#     remote-copy and terraform plugins, helm (chart rendering in the kubernetes plugin) and
#     tofu (run by the terraform plugin);
#   - server-ansible (--target server-ansible): the server image plus Python, ansible-core, the
#     ansible.posix and community.general collections and the external ansible plugin, for the
#     workers that serve the ansible plugin's queue (dbos.plugins); not distroless;
#   - client (--target client): only the manent client, which talks to the API.
# Web interface: vite build writes to internal/webui/dist, embedded by go:embed.
FROM node:22.11.0-bookworm-slim AS web
WORKDIR /src/web
COPY web/package.json web/package-lock.json ./
RUN npm ci --no-audit --no-fund
COPY api /src/api
COPY web ./
RUN npm run build

FROM golang:1.27 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=web /src/internal/webui/dist internal/webui/dist
ARG VERSION=dev
ARG COMMIT=
RUN LDFLAGS="-s -w -X github.com/manent-deployer/manent/internal/version.Version=${VERSION} -X github.com/manent-deployer/manent/internal/version.Commit=${COMMIT}" \
 && CGO_ENABLED=0 go build -trimpath -ldflags "$LDFLAGS" -o /out/manent ./cmd/manent \
 && CGO_ENABLED=0 go build -trimpath -ldflags "$LDFLAGS" -o /out/manent-server ./cmd/manent-server \
 && CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/manent-plugin-remote-copy ./cmd/manent-plugin-remote-copy
# The terraform plugin lives in its own repository (manent-deployer/manent-plugin-terraform).
ARG TERRAFORM_PLUGIN_VERSION=v0.1.0
RUN CGO_ENABLED=0 GOBIN=/out go install -trimpath -ldflags "-s -w" \
    github.com/manent-deployer/manent-plugin-terraform@${TERRAFORM_PLUGIN_VERSION}
# The ansible plugin lives in its own repository too (manent-deployer/manent-plugin-ansible).
ARG ANSIBLE_PLUGIN_VERSION=v0.1.0
RUN CGO_ENABLED=0 GOBIN=/out go install -trimpath -ldflags "-s -w" \
    github.com/manent-deployer/manent-plugin-ansible@${ANSIBLE_PLUGIN_VERSION}

# Static Helm binary, used only for "helm template" by the kubernetes plugin.
FROM alpine/helm:4.3.0 AS helm

# Static OpenTofu binary, run by the terraform plugin.
FROM ghcr.io/opentofu/opentofu:1.13.2-minimal AS tofu

FROM gcr.io/distroless/static-debian12:nonroot AS client
COPY --from=build /out/manent /usr/local/bin/
USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/manent"]

FROM gcr.io/distroless/static-debian12:nonroot AS server
COPY --from=build /out/manent-server /out/manent /out/manent-plugin-remote-copy /out/manent-plugin-terraform /usr/local/bin/
COPY --from=helm /usr/bin/helm /usr/local/bin/helm
COPY --from=tofu /usr/local/bin/tofu /usr/local/bin/tofu
WORKDIR /var/lib/manent
ENV MANENT_CONFIG=/etc/manent/manent.yml
# 8080: API; 9090: /metrics, /healthz, /readyz (observability.listen).
EXPOSE 8080 9090
USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/manent-server"]
CMD ["serve"]

# Workers that serve the ansible plugin: Python and ansible-core are needed on the machine that
# runs the plugin. ANSIBLE_COLLECTIONS adds collections to the image; the artifacts can also
# bring their own (collections/ in the project).
FROM python:3.13-slim-bookworm AS server-ansible
ARG ANSIBLE_CORE_VERSION=2.21.5
ARG ANSIBLE_COLLECTIONS="ansible.posix:2.2.2 community.general:13.5.0"
RUN apt-get update && apt-get install -y --no-install-recommends openssh-client \
 && rm -rf /var/lib/apt/lists/* \
 && pip install --no-cache-dir "ansible-core==${ANSIBLE_CORE_VERSION}" \
 && ansible-galaxy collection install -p /usr/share/ansible/collections ${ANSIBLE_COLLECTIONS} \
 && useradd --uid 65532 --create-home --home-dir /home/nonroot nonroot \
 && mkdir -p /var/lib/manent && chown nonroot /var/lib/manent
COPY --from=build /out/manent-server /out/manent /out/manent-plugin-remote-copy /out/manent-plugin-terraform /out/manent-plugin-ansible /usr/local/bin/
COPY --from=helm /usr/bin/helm /usr/local/bin/helm
COPY --from=tofu /usr/local/bin/tofu /usr/local/bin/tofu
WORKDIR /var/lib/manent
ENV MANENT_CONFIG=/etc/manent/manent.yml
EXPOSE 8080 9090
USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/manent-server"]
CMD ["worker"]
