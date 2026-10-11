# Immagine di manent per i casi d'uso: usa i binari linux compilati da run.sh in .work/bin,
# così non serve l'immagine golang. Stessa base e stesso utente dell'immagine ufficiale.
FROM gcr.io/distroless/static-debian12:nonroot
COPY manent-server manent /usr/local/bin/
WORKDIR /tmp
ENV MANENT_CONFIG=/etc/manent/manent.yml
USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/manent-server"]
CMD ["serve"]
