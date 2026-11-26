FROM golang:1.26-alpine AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG SERVICE=gateway
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/service ./cmd/${SERVICE}

FROM alpine:3.21
ARG INSTALL_RUNTIMES=false
RUN if [ "$INSTALL_RUNTIMES" = "true" ]; then apk add --no-cache python3 nodejs; fi \
    && adduser -D -u 10001 app
USER app
COPY --from=builder /out/service /service
ENTRYPOINT ["/service"]
