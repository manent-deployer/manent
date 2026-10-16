# HAProxy with the Data Plane API for UC10. The configuration is writable by the haproxy user:
# the Data Plane API rewrites it when manent changes the server weights.
FROM haproxytech/haproxy-debian:3.0.6
USER root
RUN mkdir -p /var/run/haproxy /tmp/dataplaneapi && chown -R haproxy:haproxy /var/run/haproxy /tmp/dataplaneapi /usr/local/etc/haproxy
COPY --chown=haproxy:haproxy haproxy.cfg dataplaneapi.yml /usr/local/etc/haproxy/
COPY entrypoint.sh /entrypoint.sh
USER haproxy
EXPOSE 80 5555
ENTRYPOINT ["/entrypoint.sh"]
CMD []
