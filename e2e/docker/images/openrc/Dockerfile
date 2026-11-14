FROM alpine:3.21
# DL3018: packages float within the pinned base release: apk drops superseded versions from the mirror, so exact pins break the build after any security update
# hadolint ignore=DL3018
RUN set -eu; for attempt in 1 2 3; do apk add --no-cache openrc && break; [ "$attempt" -lt 3 ] || exit 1; sleep 2; done \
    && printf '%s\n' 'rc_sys="docker"' 'rc_provide="net"' >> /etc/rc.conf \
    && printf '%s\n' '::sysinit:/bin/mkdir -p /run/openrc' '::sysinit:/bin/touch /run/openrc/softlevel' '::wait:/sbin/openrc default' '::shutdown:/sbin/openrc shutdown' > /etc/inittab
ENTRYPOINT ["/sbin/init"]
