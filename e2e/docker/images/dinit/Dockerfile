FROM alpine:3.21
# DL3018: packages float within the pinned base release: apk drops superseded versions from the mirror, so exact pins break the build after any security update
# hadolint ignore=DL3018
RUN set -eu; for attempt in 1 2 3; do apk add --no-cache dinit && break; [ "$attempt" -lt 3 ] || exit 1; sleep 2; done \
    && mkdir -p /etc/dinit.d/boot.d \
    && printf '%s\n' 'type = internal' 'waits-for.d = boot.d' > /etc/dinit.d/boot
ENTRYPOINT ["/usr/bin/env", "dinit", "--system", "--container", "--services-dir", "/etc/dinit.d"]
