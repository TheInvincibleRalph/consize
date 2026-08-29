# Consize engine — one multi-stage build per binary, selected by BIN.
#
#   docker build --build-arg BIN=collector -t consize/collector:latest .
#
# The image is static (CGO off), distroless, non-root: the k8s manifests
# (deploy/*.yaml) run it with a least-privilege ServiceAccount, and the
# verifier writes nothing to disk — the audit trail lives in Postgres.
ARG BIN

FROM golang:1.26-alpine AS build
ARG BIN
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# GOARCH pinned: dev machines are Apple Silicon (arm64); the target
# cluster is amd64, and a mismatched platform fails the kubelet pull
# ("no match for platform in manifest").
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o /out/consize ./cmd/${BIN}

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/consize /consize
USER nonroot:nonroot
ENTRYPOINT ["/consize"]
