ARG BASE_IMAGE=docker.io/ubuntu:26.04
ARG BUILD_IMAGE=docker.io/library/golang:1.26
ARG HTTP_PROXY
ARG HTTPS_PROXY

FROM ${BASE_IMAGE} AS zeccni-base

# Re-declared on purpose: an ARG before the first FROM is only in scope for the
# FROM lines themselves, so the previous "ENV HTTP_PROXY $HTTP_PROXY" here
# expanded to an empty string and the proxy was never actually applied.
ARG HTTP_PROXY
ARG HTTPS_PROXY
ENV HTTP_PROXY=$HTTP_PROXY
ENV HTTPS_PROXY=$HTTPS_PROXY

ENV DEBIAN_FRONTEND=noninteractive
RUN echo 'debconf debconf/frontend select Noninteractive' | debconf-set-selections && \
    apt-get update &&\
    apt-get install -y apt-utils kmod libelf1 libmnl0 iptables nftables wget && \
    apt-get purge --auto-remove && apt-get clean && rm -rf /var/lib/apt/lists/*

FROM ${BUILD_IMAGE} AS cni-plugins-builder

ARG GOPROXY
ARG HTTP_PROXY
ARG HTTPS_PROXY
ENV GOPROXY=$GOPROXY
ENV HTTP_PROXY=$HTTP_PROXY
ENV HTTPS_PROXY=$HTTPS_PROXY
WORKDIR /go/src/containernetworking
RUN git clone https://github.com/containernetworking/plugins.git --depth=1 --branch v1.2.0
RUN cd plugins && bash build_linux.sh

FROM ${BUILD_IMAGE} AS zec-builder
ARG GOPROXY
ARG HTTP_PROXY
ARG HTTPS_PROXY
ENV GOPROXY=$GOPROXY
ENV HTTP_PROXY=$HTTP_PROXY
ENV HTTPS_PROXY=$HTTPS_PROXY
# .git is excluded from the build context, so the git commands in the Makefile
# find nothing here. Without these the binaries get stamped ".@" / "DEFAULT".
ARG VERSION
ARG COMMIT_ID
ARG BRANCH
WORKDIR /go/src/zenlayer-vpc-cni-k8s
COPY . .
RUN make bin VERSION=$VERSION COMMIT_ID=$COMMIT_ID BRANCH=$BRANCH

FROM zeccni-base
# Clear the build-time proxy so it does not leak into the runtime image.
ENV HTTP_PROXY=""
ENV HTTPS_PROXY=""
ENV GOPROXY=""
COPY --from=zec-builder /go/src/zenlayer-vpc-cni-k8s/output /zenlayer
# loopback is required of every CNI plugin; portmap is what makes a hostPort in
# a Pod spec do anything. install_env.sh copies this whole directory, so a
# binary added here lands in /opt/cni/bin without touching the script.
COPY --from=cni-plugins-builder /go/src/containernetworking/plugins/bin/loopback \
     /go/src/containernetworking/plugins/bin/portmap \
     /containernetworking/plugins/
RUN ln -s /zenlayer/zec-cni /bin/zec-cni && \
    ln -s /zenlayer/zec-ipam /bin/zec-ipam

RUN mkdir -p /script
COPY script/bootstrap/* /script/
RUN chmod +x /script/*.sh

