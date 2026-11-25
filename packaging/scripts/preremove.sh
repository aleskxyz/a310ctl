#!/bin/sh
# Stop and disable the service on removal only, not on upgrade.
# deb: $1 = remove | upgrade; rpm: $1 = 0 (erase) | 1 (upgrade)
if [ "$1" = "remove" ] || [ "$1" = "0" ]; then
    if command -v systemctl >/dev/null 2>&1; then
        systemctl disable --now a310ctl.service >/dev/null 2>&1 || true
    fi
fi
exit 0
