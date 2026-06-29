# ---- build stage ----
FROM golang:1.26-alpine AS build
WORKDIR /src

ENV GOPROXY=https://goproxy.cn,direct \
    GOSUMDB=off

COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download

COPY . .
# Persist the module + compile caches across builds (BuildKit cache mounts) so a
# rebuild after a small code change recompiles only the changed packages instead
# of every dependency from scratch — turns a ~2min full build into seconds.
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /bin/server ./cmd/server
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /bin/migrate ./cmd/migrate

# ---- runtime stage ----
FROM alpine:3.20
RUN apk add --no-cache ca-certificates && adduser -D -u 10001 app
USER app
COPY --from=build /bin/server /bin/server
COPY --from=build /bin/migrate /bin/migrate
EXPOSE 8080
ENTRYPOINT ["/bin/server"]
