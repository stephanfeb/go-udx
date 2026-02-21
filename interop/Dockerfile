FROM golang:1.24-bookworm

# Install tc (traffic control) for network simulation
RUN apt-get update && apt-get install -y iproute2 && rm -rf /var/lib/apt/lists/*

WORKDIR /build

# Copy all three local modules
COPY go-udx/ /build/go-udx/
COPY go-libp2p-udx-transport/ /build/go-libp2p-udx-transport/
COPY dart-libp2p/interop/go-peer/ /build/go-peer/

# Patch go.mod replace directives to point to /build paths
WORKDIR /build/go-libp2p-udx-transport
RUN sed -i 's|../go-udx|/build/go-udx|g' go.mod && go mod tidy

WORKDIR /build/go-peer
RUN sed -i 's|../../../go-libp2p-udx-transport|/build/go-libp2p-udx-transport|g' go.mod && \
    sed -i 's|../../../go-udx|/build/go-udx|g' go.mod && \
    go mod tidy && \
    go build -o /go-peer .

EXPOSE 55223/udp

# Default: run as UDX echo server on fixed port
ENTRYPOINT ["/go-peer"]
CMD ["--mode=echo-server", "--transport=udx", "--port=55223"]
