FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/screenjson-db-importer .

FROM alpine:3.22
RUN apk add --no-cache ca-certificates
WORKDIR /data
COPY --from=build /out/screenjson-db-importer /usr/local/bin/screenjson-db-importer
ENTRYPOINT ["/usr/local/bin/screenjson-db-importer"]
