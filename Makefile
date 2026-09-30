IMG ?= glb-gateway-controller:dev
GOPROXY ?= https://proxy.golang.org,direct

.PHONY: fmt vet test build docker-build manifests install uninstall

fmt:
	gofmt -w $$(find api cmd internal -name '*.go' -type f)

vet:
	GOPROXY=$(GOPROXY) go vet ./...

test:
	GOPROXY=$(GOPROXY) go test ./... -cover

build:
	GOPROXY=$(GOPROXY) CGO_ENABLED=0 go build -o bin/controller ./cmd/controller

docker-build:
	docker build --build-arg GOPROXY=$(GOPROXY) -t $(IMG) .

manifests:
	controller-gen crd paths=./api/... output:crd:artifacts:config=config/crd/bases
	controller-gen rbac:roleName=manager-role paths=./internal/controller/... output:rbac:artifacts:config=config/rbac

install:
	kubectl apply -f config/crd/bases

uninstall:
	kubectl delete -f config/crd/bases --ignore-not-found
