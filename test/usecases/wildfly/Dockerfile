# WildFly for UC14 (jboss plugin): a management user for manent and management interface on
# all addresses (http, port 9990). standalone.sh handles shutdown(restart=true).
FROM quay.io/wildfly/wildfly:33.0.2.Final-jdk21
RUN /opt/jboss/wildfly/bin/add-user.sh -u manent -p uc-jboss-1 --silent
EXPOSE 8080 9990
CMD ["/opt/jboss/wildfly/bin/standalone.sh", "-b", "0.0.0.0", "-bmanagement", "0.0.0.0"]
