#!/bin/sh
set -e
BR=br-yt
rule() { iptables -C "$@" 2>/dev/null || iptables -I "$@"; }
for net in 10.0.0.0/8 172.16.0.0/12 192.168.0.0/16 100.64.0.0/10 169.254.0.0/16 224.0.0.0/4; do
  rule DOCKER-USER -i $BR -d $net -j DROP
done
rule DOCKER-USER -i $BR -m conntrack --ctstate ESTABLISHED,RELATED -j RETURN
rule DOCKER-USER -i $BR -o $BR -j RETURN
rule INPUT -i $BR -m conntrack --ctstate NEW -j DROP
