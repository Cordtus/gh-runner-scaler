#!/usr/bin/env bash
set -euo pipefail

template=${1:-gh-runner-template}

if ! /snap/bin/lxc info "$template" | grep -q 'Status: STOPPED'; then
  echo "refusing to mutate running template $template" >&2
  exit 1
fi

/snap/bin/lxc start "$template"
trap '/snap/bin/lxc stop "$template" --force >/dev/null 2>&1 || true' EXIT
/snap/bin/lxc exec "$template" -- bash -ceu '
systemctl disable --now promtail.service 2>/dev/null || true
rm -rf /home/runner/_diag/* /home/runner/_work/*
install -d -m 0755 /var/lib/promtail /etc/gh-runner-observability
printf "%s\n" "[Unit]" "Description=Promtail log shipper for the current ephemeral runner" "After=network-online.target" "" "[Service]" "Type=simple" "ExecStart=/usr/local/bin/promtail -config.file=/etc/gh-runner-observability/promtail.yml" "Restart=no" "" "[Install]" "WantedBy=multi-user.target" > /etc/systemd/system/promtail.service
systemctl daemon-reload
systemctl disable promtail.service
'
/snap/bin/lxc stop "$template"
trap - EXIT
echo "prepared stopped template $template for managed runner observability"
