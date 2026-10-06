FROM golang:1.22-alpine AS build
WORKDIR /src
RUN apk add --no-cache ca-certificates
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# go.mod lists only direct deps; tidy fills in the indirect ones.
RUN go mod tidy
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" \
    -o /out/gateway ./cmd/gateway

FROM alpine:3.20
RUN apk add --no-cache ca-certificates wget && adduser -D -u 65532 app
COPY --from=build /out/gateway /usr/local/bin/gateway
USER app
EXPOSE 8080
ENTRYPOINT ["gateway"]
