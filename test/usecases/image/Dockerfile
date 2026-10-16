# manent image for the use cases: it uses the linux binaries built by run.sh in .work/bin,
# so the golang image is not needed. Same base and same user as the official image.
FROM gcr.io/distroless/static-debian12:nonroot
COPY manent-server manent manent-uc-oidc /usr/local/bin/
WORKDIR /tmp
ENV MANENT_CONFIG=/etc/manent/manent.yml
USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/manent-server"]
CMD ["serve"]
