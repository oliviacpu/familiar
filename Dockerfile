# familiar as a container: the headless Linux build, reporting the host it runs
# on. Run with the host's network (for its network rates) and, for an NVIDIA
# GPU, the NVIDIA runtime. Mount any other drives read-only and list them in
# FAMILIAR_DISKS.
FROM --platform=$BUILDPLATFORM golang:1.27 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
ARG TARGETOS TARGETARCH
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /familiar .

# distroless/base rather than static: the nvidia-smi that the NVIDIA runtime
# puts in the container needs glibc.
FROM gcr.io/distroless/base-debian12:nonroot
COPY --from=build /familiar /familiar
# Images update by pulling a new tag, not from inside.
ENV FAMILIAR_AUTO_UPDATE=false
LABEL org.opencontainers.image.source="https://github.com/oliviacpu/familiar" \
      org.opencontainers.image.description="Reports a computer's CPU, GPU, memory, drives, drive health, temperatures and network to Home Assistant" \
      coolify.managed="true"
ENTRYPOINT ["/familiar"]
CMD ["run"]
