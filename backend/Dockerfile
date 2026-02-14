FROM golang:1.24-alpine AS build

WORKDIR /code
COPY go.mod go.sum /code/
RUN go mod download

COPY *.go /code/
RUN go build -o /code/server

FROM ubuntu:24.04 AS runtime

RUN apt update && apt install -y \
    wget \
    golang-go \
    nodejs \
    npm \
    python3 \
    python3-pip \
    openjdk-11-jdk \
    build-essential \
    libcap-dev \
    pkg-config \
    libsystemd-dev

# Install isolate (https://github.com/ioi/isolate)
RUN wget -O /tmp/isolate.tar.gz https://github.com/ioi/isolate/archive/refs/tags/${ISOLATE_VERSION}.tar.gz \
    && tar -xzvf /tmp/isolate.tar.gz -C /tmp \
    && mkdir -p /usr/local/etc \
    && make -C /tmp/isolate-${ISOLATE_VERSION#v} isolate default.cf \
    && cp /tmp/isolate-${ISOLATE_VERSION#v}/default.cf /usr/local/etc/isolate \
    && install /tmp/isolate-${ISOLATE_VERSION#v}/isolate /usr/local/bin/isolate

# Create a sandbox directory
RUN mkdir /code

WORKDIR /app
COPY --from=build /code/server /app/server

EXPOSE 8000

CMD ["/app/server"]
