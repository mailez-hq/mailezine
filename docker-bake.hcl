# docker-bake.hcl — build the mailezine community engine image (CE) from
# the mailezine repo itself.
#
# The EE variant lives in docker-bake.ee.hcl (private repo only); build it
# with both files:
#   docker buildx bake -f docker-bake.hcl -f docker-bake.ee.hcl ee
#
# The mailez control plane consumes these images as
# ghcr.io/mailez-hq/mailez-mailezine[-ee] (REGISTRY below must stay aligned
# with the mailez repo's docker-bake.hcl).
#
# Examples:
#   docker buildx bake                                  # CE, tag :local
#   VERSION=v1.2.3 APK_MIRROR=dl-cdn.alpinelinux.org \
#     PLATFORMS=linux/amd64,linux/arm64 docker buildx bake ce --push

variable "VERSION" {
  # Image tag and the -ldflags version baked into the binary.
  default = "local"
}

variable "REGISTRY" {
  default = "ghcr.io/mailez-hq"
}

variable "PLATFORMS" {
  default = "linux/amd64"
}

variable "APK_MIRROR" {
  # Alpine apk mirror for the runtime stage; release CI overrides it with
  # dl-cdn.alpinelinux.org.
  default = "mirrors.aliyun.com"
}

group "default" {
  targets = ["ce"]
}

group "ce" {
  targets = ["mailezine-ce"]
}

target "mailezine-ce" {
  context = "."
  dockerfile = "Dockerfile"
  args = { MAILEZ_EDITION = "ce", VERSION = VERSION, APK_MIRROR = APK_MIRROR }
  tags = ["${REGISTRY}/mailez-mailezine:${VERSION}"]
  platforms = split(",", PLATFORMS)
}
