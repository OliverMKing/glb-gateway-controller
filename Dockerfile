FROM golang:1.25.8 AS builder
ARG GOPROXY=https://proxy.golang.org,direct
WORKDIR /workspace
COPY go.mod go.sum* ./
RUN GOPROXY=${GOPROXY} go mod download
COPY api api
COPY cmd cmd
COPY internal internal
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags='-s -w' -o controller ./cmd/controller

FROM gcr.io/distroless/static:nonroot
WORKDIR /
COPY --from=builder /workspace/controller /controller
USER 65532:65532
ENTRYPOINT ["/controller"]
