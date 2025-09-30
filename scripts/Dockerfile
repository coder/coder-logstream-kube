FROM --platform=$BUILDPLATFORM scratch AS base
ARG TARGETARCH
COPY --chmod=0555 ./coder-logstream-kube-${TARGETARCH} /coder-logstream-kube
USER 65532:65532
ENTRYPOINT ["/coder-logstream-kube"]