# Immagini di manent, statiche (SQLite è in Go puro) e senza shell, eseguite come utente non root:
#   - server (default): manent-server (API, worker, admin), il client manent e il plugin esterno remote-copy;
#   - client (--target client): solo il client manent, che parla con l'API.
# Interfaccia web: vite build scrive in internal/webui/dist, incorporata da go:embed.
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
RUN LDFLAGS="-s -w -X github.com/joeg-ita/manent/internal/version.Version=${VERSION} -X github.com/joeg-ita/manent/internal/version.Commit=${COMMIT}" \
 && CGO_ENABLED=0 go build -trimpath -ldflags "$LDFLAGS" -o /out/manent ./cmd/manent \
 && CGO_ENABLED=0 go build -trimpath -ldflags "$LDFLAGS" -o /out/manent-server ./cmd/manent-server \
 && CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/manent-plugin-remote-copy ./cmd/manent-plugin-remote-copy

FROM gcr.io/distroless/static-debian12:nonroot AS client
COPY --from=build /out/manent /usr/local/bin/
USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/manent"]

FROM gcr.io/distroless/static-debian12:nonroot AS server
COPY --from=build /out/manent-server /out/manent /out/manent-plugin-remote-copy /usr/local/bin/
WORKDIR /var/lib/manent
ENV MANENT_CONFIG=/etc/manent/manent.yml
# 8080: API; 9090: /metrics, /healthz, /readyz (observability.listen).
EXPOSE 8080 9090
USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/manent-server"]
CMD ["serve"]
