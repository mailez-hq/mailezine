# docker-bake.hcl — build the mailezine engine image from the mailezine
# repo itself.
#
# The mailez control plane consumes these images as
# ghcr.io/mailez-hq/mailez-mailezine (REGISTRY below must stay aligned
# with the mailez repo's docker-bake.hcl).
#
# Examples:
#   docker buildx bake                                  # tag :local
#   VERSION=v1.2.3 APK_MIRROR=dl-cdn.alpinelinux.org \
#     PLATFORMS=linux/amd64,linux/arm64 docker buildx bake --push

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
  targets = ["mailezine"]
}

target "mailezine" {
  context = "."
  dockerfile = "Dockerfile"
  args = { VERSION = VERSION, APK_MIRROR = APK_MIRROR }
  tags = ["${REGISTRY}/mailez-mailezine:${VERSION}"]
  platforms = split(",", PLATFORMS)
}
