FROM golang:1.25-bookworm AS build

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

FROM quay.io/ramalama/ramalama:latest

COPY --from=build /out/dra-node /usr/local/bin/dra-node
COPY --from=build /out/apple-vulkan-probe /usr/local/bin/apple-vulkan-probe
COPY --from=build /out/probe.comp.spv /usr/local/lib/apple-vulkan/probe.comp.spv
ENTRYPOINT ["/usr/local/bin/dra-node"]
