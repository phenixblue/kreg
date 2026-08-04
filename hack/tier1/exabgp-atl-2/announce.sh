#!/bin/sh
# Simulates atl-2: weight 20 (community 1:20), tier canary (community
# 2:1), serviceTag 80 (community 4:80) — same service as atl-1, so both
# show up as active backends behind the same BGPBackendPolicy.
mkfifo /tmp/cmds 2>/dev/null
echo 'announce route 198.51.100.70/32 next-hop self large-community [4200000000:1:20 4200000000:2:1 4200000000:4:80]'
while true; do
  if read -r line < /tmp/cmds; then
    echo "$line"
  fi
done
