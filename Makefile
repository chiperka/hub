VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
IMAGE   ?= chiperka-hub

.PHONY: ui build run dev test docker docker-with-worker clean

ui:
	cd web && npm ci --no-audit --no-fund && npm run build

build: ui
	CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=$(VERSION)" -o chiperka-hub .

# Run locally without auth, data in ./data. Add CHIPERKA_LOCAL_WORKER=1 (chiperka-worker in PATH) to execute tests here.
run: build
	CHIPERKA_AUTH=disabled ./chiperka-hub

# UI with hot reload, proxying the API to a server on :8080
dev:
	cd web && npm run dev

test:
	go vet ./... && go test ./...

docker:
	docker build --build-arg VERSION=$(VERSION) -t $(IMAGE) .

# make docker-with-worker WORKER_IMAGE=<chiperka-worker image>
docker-with-worker:
	docker build --target hub-with-worker --build-arg VERSION=$(VERSION) --build-arg WORKER_IMAGE=$(WORKER_IMAGE) -t $(IMAGE) .

clean:
	rm -rf chiperka-hub web/dist/assets web/dist/index.html web/dist/favicon.svg
