# manent images, static (SQLite is pure Go) and without a shell, run as a non-root user:
#   - server (default): manent-server (API, worker, admin), the manent client, the external
#     remote-copy plugin and helm (chart rendering in the kubernetes plugin);
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

# Static Helm binary, used only for "helm template" by the kubernetes plugin.
FROM alpine/helm:4.3.0 AS helm

FROM gcr.io/distroless/static-debian12:nonroot AS client
COPY --from=build /out/manent /usr/local/bin/
USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/manent"]

FROM gcr.io/distroless/static-debian12:nonroot AS server
COPY --from=build /out/manent-server /out/manent /out/manent-plugin-remote-copy /usr/local/bin/
COPY --from=helm /usr/bin/helm /usr/local/bin/helm
WORKDIR /var/lib/manent
ENV MANENT_CONFIG=/etc/manent/manent.yml
# 8080: API; 9090: /metrics, /healthz, /readyz (observability.listen).
EXPOSE 8080 9090
USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/manent-server"]
CMD ["serve"]
