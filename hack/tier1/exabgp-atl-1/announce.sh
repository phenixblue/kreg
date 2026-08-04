#!/bin/sh
# Simulates atl-1 from docs/design/architecture.md's worked example:
# weight 80 (community 1:80), serviceTag 80 (community 4:80).
mkfifo /tmp/cmds 2>/dev/null
echo 'announce route 198.51.100.10/32 next-hop self large-community [4200000000:1:80 4200000000:4:80]'
while true; do
  if read -r line < /tmp/cmds; then
    echo "$line"
  fi
done
