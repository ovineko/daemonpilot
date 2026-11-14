FROM debian:bookworm-slim
# DL3008: packages float within the pinned base release: apt drops superseded versions from the mirror, so exact pins break the build after any security update
# hadolint ignore=DL3008
RUN set -eu; for attempt in 1 2 3 4 5; do apt-get update && apt-get install -y --no-install-recommends sysvinit-core sysv-rc procps && break; [ "$attempt" -lt 5 ] || exit 1; sleep 5; done; rm -rf /var/lib/apt/lists/* \
    && printf '%s\n' 'id:2:initdefault:' 'rc:2:wait:/etc/init.d/rc 2' 'l0:0:wait:/etc/init.d/rc 0' 'l6:6:wait:/etc/init.d/rc 6' > /etc/inittab
ENTRYPOINT ["/sbin/init"]
