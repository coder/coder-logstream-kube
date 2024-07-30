FROM --platform=$BUILDPLATFORM scratch AS base
ARG TARGETARCH
COPY ./coder-logstream-kube-${TARGETARCH} /coder-logstream-kube
ENTRYPOINT ["/coder-logstream-kube"]