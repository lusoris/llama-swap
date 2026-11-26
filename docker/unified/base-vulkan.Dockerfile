# Builder base for Vulkan builds. See base-cuda.Dockerfile for why the builder
# base is a published image rather than a stage in a shared Dockerfile.

FROM ubuntu:24.04@sha256:562111b88caa79c71391c0c4d3c2114a7d5c35915c20561fd893cb1e07929e6b

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
