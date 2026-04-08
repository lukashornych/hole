#!/bin/sh
set -eu

host_ip=$(getent hosts host.internal | awk '{print $1}')
sed "s/{HOST_GATEWAY_IP}/${host_ip}/g" /etc/coredns/Corefile.template > /tmp/Corefile

exec coredns -conf /tmp/Corefile
