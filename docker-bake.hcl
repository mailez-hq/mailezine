# docker-bake.hcl — build the mailezine engine images (CE + EE variants)
# from the mailezine repo itself.
#
# The mailez control plane consumes these images as
# ghcr.io/mailez-hq/mailez-mailezine[-ee] (REGISTRY below must stay aligned
# with the mailez repo's docker-bake.hcl).
#
# Examples:
#   docker buildx bake                                  # CE, tag :local
#   docker buildx bake ee                               # EE variant
#   VERSION=v1.2.3 APK_MIRROR=dl-cdn.alpinelinux.org \
#     PLATFORMS=linux/amd64,linux/arm64 docker buildx bake ce ee --push

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

group "ee" {
  targets = ["mailezine-ee"]
}

target "mailezine-ce" {
  context = "."
  dockerfile = "Dockerfile"
  args = { MAILEZ_EDITION = "ce", VERSION = VERSION, APK_MIRROR = APK_MIRROR }
  tags = ["${REGISTRY}/mailez-mailezine:${VERSION}"]
  platforms = split(",", PLATFORMS)
}

target "mailezine-ee" {
  context = "."
  dockerfile = "Dockerfile"
  args = { MAILEZ_EDITION = "ee", VERSION = VERSION, APK_MIRROR = APK_MIRROR }
  tags = ["${REGISTRY}/mailez-mailezine-ee:${VERSION}"]
  platforms = split(",", PLATFORMS)
}
