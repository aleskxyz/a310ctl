#!/bin/sh
# deb: $1 = configure, $2 = previous version (empty on a fresh install)
# rpm: $1 = 1 on a fresh install, 2 on an upgrade
command -v systemctl >/dev/null 2>&1 || exit 0
running=false
[ -d /run/systemd/system ] && running=true   # not in a chroot / image build
$running && systemctl daemon-reload >/dev/null 2>&1

if [ "$1" = "1" ] || { [ "$1" = "configure" ] && [ -z "$2" ]; }; then
    # Fresh install: the default configuration keeps the card at factory settings,
    # so the service can be enabled and started right away.
    if $running; then
        systemctl enable --now a310ctl.service >/dev/null 2>&1 || true
        sleep 1
        if systemctl is-active --quiet a310ctl.service; then
            echo "a310ctl: service running with factory settings; edit /etc/a310ctl/config.yaml to change them"
        else
            echo "a310ctl: WARNING: the service did not start; see: journalctl -u a310ctl" >&2
        fi
    else
        systemctl enable a310ctl.service >/dev/null 2>&1 || true
        echo "a310ctl: service enabled; it starts at the next boot"
    fi
elif $running && systemctl is-active --quiet a310ctl.service; then
    systemctl restart a310ctl.service || true
fi
exit 0
