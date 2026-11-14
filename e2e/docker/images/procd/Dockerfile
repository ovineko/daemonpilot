FROM openwrt/rootfs:x86-64-24.10.8
RUN mkdir -p /var/lock /var/run \
    && printf '%s\n' '::sysinit:/etc/init.d/rcS S boot' '::shutdown:/etc/init.d/rcS K shutdown' > /etc/inittab
ENTRYPOINT ["/sbin/procd"]
