FROM alpine:3.21
# SC2016: $script is expanded later by the generated rcS, not by this RUN
# hadolint ignore=SC2016
RUN mkdir -p /etc/init.d \
    && printf '%s\n' '#!/bin/sh' 'for script in /etc/init.d/S*; do' '  [ ! -x "$script" ] || "$script" start' 'done' > /etc/init.d/rcS \
    && chmod +x /etc/init.d/rcS \
    && printf '%s\n' '::sysinit:/etc/init.d/rcS' > /etc/inittab
ENTRYPOINT ["/sbin/init"]
