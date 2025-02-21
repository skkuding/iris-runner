# IRIS RUNNER
FROM alpine:latest

RUN apk update && apk add --no-cache \
    go \
    nodejs \
    npm \
    python3 \
    py3-pip \
    openjdk11 \
    build-base \
    git

WORKDIR /app

COPY go.mod go.sum /app/

RUN go mod download

COPY . /app

RUN go build -o server main.go

EXPOSE 8000

CMD ["./server"]
