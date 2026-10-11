FROM golang:1.27-alpine AS build

WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download

COPY . .
ARG VERSION=dev
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/vigia ./cmd/vigia

FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/vigia /vigia
USER nonroot:nonroot
EXPOSE 8080
ENV VIGIA_HTTP_ADDR=:8080
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 CMD ["/vigia", "healthcheck"]
ENTRYPOINT ["/vigia"]
CMD ["serve"]
