.PHONY: build run test vet lint clean tidy

# Binary output name
BINARY_NAME=duvycrawl
BUILD_DIR=bin

# Go parameters
GOFLAGS=-trimpath
LDFLAGS=-s -w

# Build the binary
build: tidy
	@echo "Building $(BINARY_NAME)..."
	@go build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o $(BUILD_DIR)/$(BINARY_NAME).exe ./cmd/duvycrawl

# Run directly (development)
run:
	@go run ./cmd/duvycrawl -config configs/default.yaml

# Run tests
test:
	@go test -v -race ./...

# Run go vet
vet:
	@go vet ./...

# Run staticcheck (if installed)
lint: vet
	@staticcheck ./... 2>/dev/null || echo "staticcheck not installed, skipping"

# Download and tidy dependencies
tidy:
	@go mod tidy

# Remove build artifacts and database
clean:
	@rm -rf $(BUILD_DIR)
	@rm -f data/duvycrawl.db
	@echo "Cleaned build artifacts."

# Show help
help:
	@echo "Available targets:"
	@echo "  build  - Build the binary"
	@echo "  run    - Run in development mode"
	@echo "  test   - Run tests with race detector"
	@echo "  vet    - Run go vet"
	@echo "  lint   - Run vet + staticcheck"
	@echo "  tidy   - Tidy go modules"
	@echo "  clean  - Remove build artifacts"
