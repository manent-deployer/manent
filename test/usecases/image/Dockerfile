# manent image for the use cases: it uses the linux binaries built by run.sh in .work/bin,
# so the golang image is not needed. Same base, user, Helm and OpenTofu as the official image.
FROM alpine/helm:4.3.0 AS helm

FROM ghcr.io/opentofu/opentofu:1.13.2-minimal AS tofu

FROM gcr.io/distroless/static-debian12:nonroot
COPY manent-server manent manent-uc-oidc manent-uc-fakes manent-plugin-terraform /usr/local/bin/
COPY --from=helm /usr/bin/helm /usr/local/bin/helm
COPY --from=tofu /usr/local/bin/tofu /usr/local/bin/tofu
WORKDIR /tmp
ENV MANENT_CONFIG=/etc/manent/manent.yml
USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/manent-server"]
CMD ["serve"]
