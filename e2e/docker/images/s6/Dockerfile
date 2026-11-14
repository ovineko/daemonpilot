FROM alpine:3.21
# DL3018: packages float within the pinned base release: apk drops superseded versions from the mirror, so exact pins break the build after any security update
# hadolint ignore=DL3018
RUN set -eu; for attempt in 1 2 3; do apk add --no-cache s6 && break; [ "$attempt" -lt 3 ] || exit 1; sleep 2; done \
    && mkdir -p /var/service /etc/s6/sv
ENTRYPOINT ["/usr/bin/env", "s6-svscan", "-t", "500", "/var/service"]
