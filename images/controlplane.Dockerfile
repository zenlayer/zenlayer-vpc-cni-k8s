# The controlplane's own image: just zec-controlplane, for the managed control
# plane, which runs it outside the user's cluster. It needs neither the CNI
# plugins nor the networking tools of images/Dockerfile: the binary is static
# (CGO off), so scratch plus the CA bundle for the OpenAPI's TLS is enough --
# taken from the build stage, so no other base image is pulled. Built without
# debug info (DEBUG=0). Runs as nobody; a caller whose credential files are
# readable only by root (the managed control plane) sets runAsUser itself.
ARG BUILD_IMAGE=docker.io/library/golang:1.26
ARG HTTP_PROXY
ARG HTTPS_PROXY

FROM ${BUILD_IMAGE} AS zec-builder
ARG GOPROXY
ARG HTTP_PROXY
ARG HTTPS_PROXY
ENV GOPROXY=$GOPROXY
ENV HTTP_PROXY=$HTTP_PROXY
ENV HTTPS_PROXY=$HTTPS_PROXY
# .git is excluded from the build context; see images/Dockerfile.
ARG VERSION
ARG COMMIT_ID
ARG BRANCH
WORKDIR /go/src/zenlayer-vpc-cni-k8s
COPY . .
RUN make zec-controlplane DEBUG=0 VERSION=$VERSION COMMIT_ID=$COMMIT_ID BRANCH=$BRANCH

FROM scratch
COPY --from=zec-builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=zec-builder /go/src/zenlayer-vpc-cni-k8s/output/zec-controlplane /bin/zec-controlplane
USER 65534:65534
ENTRYPOINT ["/bin/zec-controlplane"]
