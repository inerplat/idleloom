FROM golang:1.25-bookworm@sha256:9b8bad821dbd3993215fb06ff9193cf34087f684ff113f391cb04c7cc3ecea15 AS build

WORKDIR /src
RUN apt-get update \
    && apt-get install -y --no-install-recommends gcc glslang-tools libvulkan-dev \
    && rm -rf /var/lib/apt/lists/*
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags='-s -w' -o /out/dra-node ./cmd/dra-node
RUN glslangValidator -V cmd/vulkan-probe/probe.comp -o /out/probe.comp.spv \
    && cc -O2 -Wall -Wextra -Werror cmd/vulkan-probe/main.c -lvulkan -o /out/apple-vulkan-probe

FROM quay.io/ramalama/ramalama@sha256:abe07beba518d6864738a67af8ea3511cdaa4b24efb197740967d0d8e19419e4

COPY --from=build /out/dra-node /usr/local/bin/dra-node
COPY --from=build /out/apple-vulkan-probe /usr/local/bin/apple-vulkan-probe
COPY --from=build /out/probe.comp.spv /usr/local/lib/apple-vulkan/probe.comp.spv
ENTRYPOINT ["/usr/local/bin/dra-node"]
