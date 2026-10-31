# manent images, static (SQLite is pure Go) and without a shell, run as a non-root user:
#   - server (default): manent-server (API, worker, admin), the manent client, the external
#     remote-copy and terraform plugins, helm (chart rendering in the kubernetes plugin) and
#     tofu (run by the terraform plugin);
#   - server-ansible (--target server-ansible): the server image plus Python, ansible-core, the
#     ansible.posix and community.general collections and the external ansible plugin, for the
#     workers that serve the ansible plugin's queue (dbos.plugins); not distroless;
#   - client (--target client): only the manent client, which talks to the API.
# Base images are pinned by digest: change tag and digest together (the digest of the
# multi-platform index, from "docker buildx imagetools inspect <image>:<tag>").
# Web interface: vite build writes to internal/webui/dist, embedded by go:embed.
FROM node:22.23.4-bookworm-slim@sha256:4b150b48b4116e2a22f2c39cc27025fbf4f153e2078fd3b14d159fdb5c7279da AS web
WORKDIR /src/web
COPY web/package.json web/package-lock.json ./
RUN npm ci --no-audit --no-fund
COPY api /src/api
COPY web ./
RUN npm run build

FROM golang:1.27.3@sha256:2920bb124f1a2e12a1d39f0cab756ae0068441ee1da7655dec7f5e374944095b AS build
WORKDIR /src
COPY go.mod go.sum ./
# Downloads from the module proxy are retried: a dropped stream (e.g. "INTERNAL_ERROR; received
# from peer") is transient and would otherwise fail the whole build.
RUN for i in 1 2 3; do go mod download && break; [ "$i" = 3 ] && exit 1; sleep $((i * 10)); done
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
RUN for i in 1 2 3; do \
      CGO_ENABLED=0 GOBIN=/out go install -trimpath -ldflags "-s -w" \
        github.com/manent-deployer/manent-plugin-terraform@${TERRAFORM_PLUGIN_VERSION} && break; \
      [ "$i" = 3 ] && exit 1; sleep $((i * 10)); \
    done
# The ansible plugin lives in its own repository too (manent-deployer/manent-plugin-ansible).
ARG ANSIBLE_PLUGIN_VERSION=v0.1.0
RUN for i in 1 2 3; do \
      CGO_ENABLED=0 GOBIN=/out go install -trimpath -ldflags "-s -w" \
        github.com/manent-deployer/manent-plugin-ansible@${ANSIBLE_PLUGIN_VERSION} && break; \
      [ "$i" = 3 ] && exit 1; sleep $((i * 10)); \
    done

# Static Helm binary, used only for "helm template" by the kubernetes plugin.
FROM alpine/helm:4.3.0@sha256:a6cf54599ccb99d90cf0712b30f03fdb3cab062e6b94e0418cc4db7e8a1464b2 AS helm

# Static OpenTofu binary, run by the terraform plugin.
FROM ghcr.io/opentofu/opentofu:1.13.2-minimal@sha256:622dc6bb98b105c18f53cf419ab64a4f3b88bd9f8c16cc978cf89e4202f4613f AS tofu

FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab AS client
COPY --from=build /out/manent /usr/local/bin/
USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/manent"]

FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab AS server
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
FROM python:3.13-slim-bookworm@sha256:281d39ff02e3b389401586134ff9e6572954687822a58055ec07a60ded4e0b7f AS server-ansible
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
