FROM debian:trixie-slim
# DL3008: packages float within the pinned base release: apt drops superseded versions from the mirror, so exact pins break the build after any security update
# hadolint ignore=DL3008
RUN set -eu; for attempt in 1 2 3 4 5; do apt-get update && apt-get install -y --no-install-recommends finit procps && break; [ "$attempt" -lt 5 ] || exit 1; sleep 5; done; rm -rf /var/lib/apt/lists/* \
    && mkdir -p /etc/finit.d/available /etc/finit.d/enabled \
    && printf '%s\n' 'runlevel 2' > /etc/finit.conf
ENTRYPOINT ["/sbin/finit"]
