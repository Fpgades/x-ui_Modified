# xui-mesh build targets.
#
# Most upstream 3x-ui workflows just call `go build`; we add a couple
# of mesh-specific helpers but don't replace the standard Go flow.

GO          ?= go
PROTOC      ?= protoc
BIN_DIR     ?= bin
BIN_NAME    ?= x-ui
BUILD_FLAGS ?= -trimpath -ldflags="-s -w"

.PHONY: all build proto clean test fmt vet tidy install-proto-tools

all: build

build:
	@mkdir -p $(BIN_DIR)
	CGO_ENABLED=1 $(GO) build $(BUILD_FLAGS) -o $(BIN_DIR)/$(BIN_NAME) main.go
	@ls -lh $(BIN_DIR)/$(BIN_NAME)

# Regenerate gRPC stubs from mesh/proto/mesh.proto. Generated files
# live under mesh/pb and ARE committed (so end users / nodes don't need
# protoc to build). Re-run this only when the .proto changes.
proto:
	@command -v $(PROTOC) >/dev/null 2>&1 || \
		{ echo "protoc not found. Run: make install-proto-tools"; exit 1; }
	@command -v protoc-gen-go >/dev/null 2>&1 || \
		{ echo "protoc-gen-go not found. Run: make install-proto-tools"; exit 1; }
	@command -v protoc-gen-go-grpc >/dev/null 2>&1 || \
		{ echo "protoc-gen-go-grpc not found. Run: make install-proto-tools"; exit 1; }
	$(PROTOC) \
		--proto_path=mesh/proto \
		--go_out=mesh/pb --go_opt=paths=source_relative \
		--go-grpc_out=mesh/pb --go-grpc_opt=paths=source_relative \
		mesh/proto/mesh.proto
	@echo "Regenerated mesh/pb/*.pb.go"

install-proto-tools:
	GOBIN=/usr/local/bin $(GO) install google.golang.org/protobuf/cmd/protoc-gen-go@latest
	GOBIN=/usr/local/bin $(GO) install google.golang.org/grpc/cmd/protoc-gen-go-grpc@latest

clean:
	rm -rf $(BIN_DIR)

test:
	$(GO) test ./...

fmt:
	$(GO) fmt ./...

vet:
	$(GO) vet ./...

tidy:
	$(GO) mod tidy
