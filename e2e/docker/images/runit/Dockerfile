FROM debian:bookworm-slim
ENV SVDIR=/var/service
# DL3008: packages float within the pinned base release: apt drops superseded versions from the mirror, so exact pins break the build after any security update
# hadolint ignore=DL3008
RUN set -eu; for attempt in 1 2 3 4 5; do apt-get update && apt-get install -y --no-install-recommends runit-init procps && break; [ "$attempt" -lt 5 ] || exit 1; sleep 5; done; rm -rf /var/lib/apt/lists/* \
    && mkdir -p /var/service /etc/runit \
    && printf '#!/bin/sh\nexit 0\n' > /etc/runit/1 \
    && printf '#!/bin/sh\nexec runsvdir /var/service\n' > /etc/runit/2 \
    && printf '#!/bin/sh\nexit 0\n' > /etc/runit/3 \
    && chmod +x /etc/runit/1 /etc/runit/2 /etc/runit/3
ENTRYPOINT ["/sbin/runit"]
