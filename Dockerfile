# I build with the toolchain image and ship on distroless: the operator holds
# cluster-wide write access to namespaces, so the smaller the surface inside the
# container, the better.
FROM golang:1.24 AS build

WORKDIR /workspace

# Dependencies first, so a code-only change does not re-download the module
# cache on every build.
COPY go.mod go.sum ./
RUN go mod download

COPY cmd/ cmd/
COPY api/ api/
COPY internal/ internal/

RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o manager ./cmd/manager

FROM gcr.io/distroless/static:nonroot
WORKDIR /
COPY --from=build /workspace/manager .
USER 65532:65532

ENTRYPOINT ["/manager"]
