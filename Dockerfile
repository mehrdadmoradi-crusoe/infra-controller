# syntax=docker/dockerfile:1.7
#
# Combined build-container + carbide-api build, for Cloud Build (no separately
# pre-built local base image needed, unlike dev/deployment/devspace/Dockerfile.api
# which expects `build-container-localdev` to already exist locally).

FROM rust:1.96.0-slim-bookworm AS builder-base

ENV RUST_NIGHTLY=nightly-2026-06-16

RUN apt-get update && \
	DEBIAN_FRONTEND=noninteractive TZ=Etc/UTC apt-get upgrade -y && \
	DEBIAN_FRONTEND=noninteractive TZ=Etc/UTC apt-get install -y \
	automake \
	binutils-aarch64-linux-gnu \
	build-essential \
	clang \
	cmake \
	curl \
	fdisk dosfstools \
	gcc-aarch64-linux-gnu \
	ipmitool \
	iproute2 \
	iputils-ping \
	jq \
	kea-dev \
	kea-dhcp4-server \
	libboost-dev \
	libgrpc-dev \
	libopenipmi-dev \
	libprotobuf-dev \
	libssh-dev \
	libssl-dev \
	libudev-dev \
	libgrpc++-dev \
	libaio-dev \
	libtss2-dev \
	lld \
	openipmi \
	pkg-config \
	protobuf-compiler-grpc \
	protobuf-compiler \
	sudo \
	tpm2-tools \
	unzip \
	wget \
	git && \
	rm -rf /var/lib/apt/lists/*

RUN update-alternatives --install /usr/bin/ld ld /usr/bin/lld 50
RUN rustup component add rustfmt

FROM builder-base AS builder

ENV CARGO_HOME=/cargo-home
ENV CARGO_NET_GIT_FETCH_WITH_CLI=true
ENV CARGO_TARGET_DIR=/cargo-target

WORKDIR /workspace

COPY . .

RUN --mount=type=cache,id=nico-native-cargo-home,target=/cargo-home,sharing=locked \
  --mount=type=cache,id=nico-native-cargo-target,target=/cargo-target,sharing=locked \
  cargo build -p carbide-api -p nico-admin-cli && \
  mkdir -p /artifacts && \
  cp /cargo-target/debug/carbide-api /cargo-target/debug/nico-admin-cli /artifacts/

FROM ubuntu:24.04

RUN apt-get update && apt-get install -y --no-install-recommends \
  ca-certificates \
  curl \
  ipmitool \
  iproute2 \
  iputils-ping \
  libudev1 \
  tpm2-tools \
  && rm -rf /var/lib/apt/lists/*

RUN mkdir -p /opt/carbide /opt/carbide/firmware /mnt/persistence /var/run/kea

COPY --from=builder /artifacts/carbide-api /opt/carbide/carbide-api
COPY --from=builder /artifacts/nico-admin-cli /opt/carbide/nico-admin-cli
RUN ln -s /opt/carbide/nico-admin-cli /opt/carbide/carbide-admin-cli
COPY crates/api/casbin-policy.csv /opt/carbide/casbin-policy.csv

ENV CASBIN_POLICY_FILE=/opt/carbide/casbin-policy.csv
