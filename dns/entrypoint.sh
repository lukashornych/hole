#!/bin/sh
set -eu

host_ip=$(getent hosts host.internal | awk '$1 !~ /:/ {print $1; exit}')
sed "s/{HOST_GATEWAY_IP}/${host_ip}/g" /etc/coredns/Corefile.template > /tmp/Corefile

exec coredns -conf /tmp/Corefile
