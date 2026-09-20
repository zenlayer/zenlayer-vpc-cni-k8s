#!/usr/bin/env bash

set -e
set -x
set -o nounset

CURDIR=$(
  cd $(dirname $0) || exit
  pwd
)

cd "${CURDIR}" || exit

# No kernel modules are required by this plugin. Pod bandwidth limiting is not
# implemented here -- see the network policy section of the README for what
# Cilium provides instead.

# install CNIs
#
# The conflist is not copied here. Its presence is what tells
# kubelet this CNI owns the node's pod networking, so publishing it before the
# daemon can answer means every pod that starts in that window hangs in
# ContainerCreating instead of falling back to another plugin -- and if the
# daemon never comes up at all (missing Secret, unreachable OpenAPI), the node
# stays that way. The daemon writes it itself once it is serving; see
# ensureCNIConfig in pkg/daemon/ipam.go.
#
# With Cilium chaining enabled the same reasoning extends one step: the conflist
# also names cilium-cni, whose ADD has no fallback when its agent is down, so
# ensureCNIConfig waits for that agent too before publishing.
/bin/cp -f /zenlayer/zec-cni /opt/cni/bin
/bin/cp -f /containernetworking/plugins/* /opt/cni/bin/

# Keep reverse-path filtering off on the pod veths. See the header of the file
# itself for why the CNI cannot do this on its own. Mounted from the host, so
# this survives the init container exiting.
#
# Not applied here: systemd-sysctl picks it up for every interface created from
# now on, and the daemon already sets the value directly on interfaces that
# exist. Running `sysctl --system` would re-apply the host's entire policy as a
# side effect, which is not this script's business.
if [ -d /etc/sysctl.d ]; then
  /bin/cp -f /script/99-zenlayer-vpc-cni.conf /etc/sysctl.d/99-zenlayer-vpc-cni.conf
  chmod 0644 /etc/sysctl.d/99-zenlayer-vpc-cni.conf
else
  echo "no /etc/sysctl.d on this host, skipping the rp_filter drop-in" >&2
fi
