# Lab worker that serves the ansible plugin's queue (UC29): the linux binaries built by run.sh
# in .work/bin, ansible-core from Debian (an older release than the official server-ansible
# image, to cover it too) and the ansible.posix collection for the JSON callback (1.x: 2.x needs a
# newer ansible-core).
FROM ghcr.io/opentofu/opentofu:1.13.2-minimal AS tofu

FROM debian:bookworm-slim
RUN apt-get update \
 && apt-get install -y --no-install-recommends ansible-core openssh-client ca-certificates \
 && rm -rf /var/lib/apt/lists/* \
 && ansible-galaxy collection install -p /usr/share/ansible/collections "ansible.posix:>=1.5.0,<2.0.0" \
 && useradd --uid 65532 --create-home --home-dir /home/nonroot nonroot
# Like the official server-ansible image, it also has the other plugins of the configuration.
COPY manent-server manent manent-plugin-ansible manent-plugin-terraform /usr/local/bin/
COPY --from=tofu /usr/local/bin/tofu /usr/local/bin/tofu
WORKDIR /tmp
ENV MANENT_CONFIG=/etc/manent/manent.yml
USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/manent-server"]
CMD ["worker"]
