# syntax=docker/dockerfile:1.4

FROM alpine:3.23.3

ARG TARGETARCH
ARG VERSION
ARG REVISION
ARG SOURCE

COPY --chmod=0755 atlas-${TARGETARCH} /atlas

RUN ln /atlas /bin/atlas

ENV ATLAS_DOCKER_IMAGE=1

LABEL maintainer="Ariga" \
      org.opencontainers.image.title="Atlas CLI" \
      org.opencontainers.image.vendor="Ariga" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${REVISION}" \
      org.opencontainers.image.source="${SOURCE}"

ENTRYPOINT ["/atlas"]
