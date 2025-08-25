# Reusable Linux verification environment for mailezine (used by the local
# cross-platform gate; the GitHub Actions jobs replicate this). Preinstalls
# the Go toolchain and C compilers so every `go test` run starts fast.
FROM debian:trixie

RUN export DEBIAN_FRONTEND=noninteractive \
    && sed -i 's|deb.debian.org|mirrors.aliyun.com|g' /etc/apt/sources.list.d/debian.sources \
    && apt-get update \
    && apt-get install -y --no-install-recommends gcc g++ ca-certificates \
    && rm -rf /var/lib/apt/lists/*

# The Go toolchain tarball is vendored next to this Dockerfile (download it
# once from https://golang.google.cn/dl/ to avoid slow in-build fetches).
COPY go.tar.gz /tmp/go.tar.gz
RUN tar -C /usr/local -xzf /tmp/go.tar.gz && rm /tmp/go.tar.gz

ENV PATH=/usr/local/go/bin:$PATH
WORKDIR /src
