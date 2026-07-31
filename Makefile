.PHONY: build frontend frontend-test test race vet check

build: frontend
	go build -trimpath -o bin/heimdall ./cmd/heimdall

frontend:
	cd web && npm ci --ignore-scripts && npm run build

frontend-test:
	cd web && npm test

test:
	go test ./...

race:
	go test -race ./...

vet:
	go vet ./...

check: test race vet
