#!/usr/bin/env bash
# Ensure the gh-runner-scaler provisions runner-class-cac runners for BOTH the
# cac-group org and the user's personal (Cordtus) org, so migrated CI jobs run.
# Run as root on the LXD host:  sudo bash deploy/fix-self-hosted-provisioning.sh
set -euo pipefail
CONFIG="/etc/gh-runner-scaler/config.toml"
if [[ "${EUID}" -ne 0 ]]; then echo "error: run as root" >&2; exit 1; fi
[[ -f "$CONFIG" ]] || { echo "error: $CONFIG missing" >&2; exit 1; }

echo "===== existing runner classes ====="
awk '/^\[\[runner_classes\]\]/{print "---"} /^(id|org|repo|prefix|labels|match_labels|max_auto_runners|enabled|template) *=/{print}' "$CONFIG"

changed=0

# 1. Ensure a personal (Cordtus) runner class that serves runner-class-cac exists,
#    with a UNIQUE prefix (prefixes must not collide across classes).
python3 - "$CONFIG" <<'PY'
import re, sys
p = sys.argv[1]
s = open(p).read()
# find the id="personal" class block and force its prefix to a unique value
has_personal = re.search(r'\[\[runner_classes\]\](?:(?!\[\[).)*?id\s*=\s*"personal"', s, re.S)
if has_personal:
    block = has_personal.group(0)
    new = re.sub(r'prefix\s*=\s*"[^"]*"', 'prefix = "gh-runner-personal"', block)
    s = s.replace(block, new)
else:
    s += '''
[[runner_classes]]
id = "personal"
org = "Cordtus"
prefix = "gh-runner-personal"
labels = "self-hosted,linux,x64,runner-class-cac"
match_labels = ["self-hosted", "linux", "x64", "runner-class-cac"]
max_auto_runners = 6
runner_work_dir = "_work"
template = "gh-runner-template"
'''
open(p, 'w').write(s)
print("personal runner class ensured (prefix=gh-runner-personal)")
PY
changed=1

# 2. Ensure the cac-group class has max_auto_runners >= 1.
if awk 'BEGIN{c=0} /^\[\[runner_classes\]\]/{id=""} /^id *=/{id=$3} id=="\"cac-group\"" && /max_auto_runners/ {v=$3; gsub(/[^0-9]/,"",v); if(v+0<1){print "needs bump"}}' "$CONFIG" | grep -q needs; then
  echo "bumping cac-group max_auto_runners to 6"
  sed -i 's/^\( *max_auto_runners *= *\)[0-9]*/\16/' "$CONFIG"
  changed=1
else
  echo "cac-group class OK"
fi

if [[ "${changed}" -eq 1 ]]; then
  echo "===== restarting scaler (config changed) ====="
  systemctl restart gh-runner-scaler
fi

echo "===== scaler status ====="
systemctl --no-pager --full status gh-runner-scaler --lines=6
echo "===== recent provisioning logs (runner-class-cac / personal / cac-group) ====="
journalctl -u gh-runner-scaler --since "20 minutes ago" --no-pager 2>/dev/null \
  | grep -iE "runner-class-cac|cac-group|personal|runner_group=personal|queued demand" | tail -20
