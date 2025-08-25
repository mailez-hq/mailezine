# Minimal legacy IMAP used to seed a real dovecot-format maildir (uidlist v3,
# ":2," flags) for the mailezine mount rehearsal.
FROM alpine:3.21
RUN sed -i 's|dl-cdn.alpinelinux.org|mirrors.aliyun.com|g' /etc/apk/repositories \
    && apk add --no-cache legacy IMAP dovecot-lmtpd
# Keep the distro default dovecot.conf (protocol registration, auth sockets)
# and layer the seeding settings last via conf.d.
COPY 99-mailez-seed.conf /etc/legacy IMAP/conf.d/99-mailez-seed.conf
CMD ["/usr/sbin/legacy IMAP", "-F"]
