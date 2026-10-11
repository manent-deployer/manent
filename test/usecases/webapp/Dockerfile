# Applicazione di prova dei casi d'uso UC6 e UC7: busybox httpd su :8080 con /version e, se
# HEALTHY=true, /healthz. Senza healthz l'HEALTHCHECK (Docker) e la readinessProbe
# (Kubernetes) restano rossi: è la release difettosa.
FROM busybox:1.36.1
ARG VERSION=0.0.0
ARG HEALTHY=true
RUN mkdir /www && echo "$VERSION" > /www/version \
 && if [ "$HEALTHY" = true ]; then echo "ok $VERSION" > /www/healthz; fi
HEALTHCHECK --interval=2s --timeout=2s --retries=2 CMD wget -q -O /dev/null http://127.0.0.1:8080/healthz || exit 1
EXPOSE 8080
CMD ["httpd", "-f", "-p", "8080", "-h", "/www"]
