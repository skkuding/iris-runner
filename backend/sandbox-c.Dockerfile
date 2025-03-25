### build stage
FROM gcc:13.3.0-bookworm AS build

COPY main.c /app/main.c
RUN gcc -o /app/main /app/main.c

### runtime stage
FROM debian:bookworm-20250317-slim AS runtime

# Create a non-root user
RUN adduser --disabled-login -u 1000 runner

COPY --from=build /app/main /app/main

RUN chown runner:runner /app/main && chmod 755 /app/main

USER runner
WORKDIR /app
CMD ["/app/main"]
