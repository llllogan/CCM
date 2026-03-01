FROM golang:1.23-alpine AS build
WORKDIR /src
COPY go.mod go.sum* ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -mod=mod -o /out/ccm ./cmd/ccm

FROM alpine:3.20
RUN apk add --no-cache ca-certificates openssh-client tzdata
RUN adduser -D -H -s /sbin/nologin ccm
WORKDIR /app
COPY --from=build /out/ccm /usr/local/bin/ccm
USER ccm
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/ccm"]
CMD ["-config", "/etc/ccm/config.yml", "-listen", ":8080"]
