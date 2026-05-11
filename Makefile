.PHONY: build run-api run-crawler test vet lint clean tidy

BUILD_DIR=bin

# Go parameters
GOFLAGS=-trimpath
LDFLAGS=-s -w

# Build both binaries
build: tidy
	@echo "Building search-api..."
	@go build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o $(BUILD_DIR)/search-api ./cmd/search-api
	@echo "Building crawler..."
	@go build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o $(BUILD_DIR)/crawler ./cmd/crawler

# Run Search API (development)
run-api:
	@go run ./cmd/search-api -config configs/default.yaml

# Run Crawler (development)
run-crawler:
	@go run ./cmd/crawler -config configs/default.yaml

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
	@rm -f data/*.db
	@echo "Cleaned build artifacts."

# Show help
help:
	@echo "Available targets:"
	@echo "  build       - Build both binaries (search-api + crawler)"
	@echo "  run-api     - Run Search API in development mode"
	@echo "  run-crawler - Run Crawler in development mode"
	@echo "  test        - Run tests with race detector"
	@echo "  vet         - Run go vet"
	@echo "  lint        - Run vet + staticcheck"
	@echo "  tidy        - Tidy go modules"
	@echo "  clean       - Remove build artifacts"
