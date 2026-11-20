# Builder base for Vulkan builds. See base-cuda.Dockerfile for why the builder
# base is a published image rather than a stage in a shared Dockerfile.

FROM ubuntu:24.04@sha256:9c7f200c07e3d4a03e7249d2c16bf66a51120fb876aa2242cf31451412543289

ENV DEBIAN_FRONTEND=noninteractive
ENV CCACHE_DIR=/ccache
ENV CCACHE_MAXSIZE=2G
ENV PATH="/usr/lib/ccache:${PATH}"

RUN apt-get update && apt-get install -y --no-install-recommends \
    build-essential cmake git python3 python3-pip libssl-dev \
    curl ca-certificates ccache make wget software-properties-common \
    libvulkan-dev glslang-tools spirv-tools vulkan-validationlayers glslc \
    pkg-config libavcodec-dev libavformat-dev libavutil-dev libswresample-dev \
    spirv-headers \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /build
