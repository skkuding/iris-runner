#API Gateway
FROM golang:1.24-alpine

RUN apk update && apk add --no-cache \
    go \
    git \
    ca-certificates \
    curl

WORKDIR /app

COPY go.mod go.sum /app/

RUN go mod download

COPY ./pod-manager.go /app

RUN go build -o pod-manager pod-manager.go

EXPOSE 8080

CMD ["./pod-manager"]